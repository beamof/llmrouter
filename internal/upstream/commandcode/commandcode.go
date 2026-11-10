package commandcode

// Command Code（commandcode.ai）订阅接入。数据面走 CLI 自己的线协议：
// POST /alpha/generate，请求体是 CLI 的 envelope（config + params），回程是
// 逐行 JSON 事件流。Go 套餐没有 Provider API，只有这条路径能用到它的额度；
// Pro / GOAT / Max 等套餐同样可用（CLI 也走这里）。
//
// 参考 magpie（github.com/yetone/magpie）与社区插件
// @magpie-community/opencode-commandcode-auth（packages/commandcode）的 Go
// 实现：出站格式对齐 command-code 1.72.2，模型目录与套餐过滤对齐其 CLI 表。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// MaxOutput / DefaultMax 输出上限（超限上游整单拒绝）
	MaxOutput  = commandCodeMaxOutput
	DefaultMax = commandCodeDefaultMax
	// CLIVersion 官方 CLI 版本（线协议对齐）
	CLIVersion            = commandCodeCLIVersion
	commandCodeKind       = "commandcode"
	commandCodeAPI        = "https://api.commandcode.ai"
	commandCodeCLIVersion = "1.72.2"
	commandCodeMaxOutput  = 200_000
	commandCodeDefaultMax = 64_000
)

// ccOf 取 Command Code 凭证（Kind 不是 commandcode 时返回 nil）
func ccOf(p Provider) *CommandCodeCred {
	if p.Kind == commandCodeKind {
		return p.CommandCode
	}
	return nil
}

// ccAPI 数据面基址（测试可注入 ccBase）
func (a *Adapter) ccAPI() string {
	if strings.TrimSpace(a.base) != "" {
		return strings.TrimRight(a.base, "/")
	}
	return commandCodeAPI
}

// ccSessionID 是进程级的 x-session-id：一个 CLI 会话在进程生命周期内保持不变。
var ccSessionID = ccNewID()

func ccNewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cc-%d", time.Now().UnixNano())
	}
	b[6] = b[6]&0x0f | 0x40 // UUID v4
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ── 本机 CLI 登录态 ──────────────────────────────────────────────────────

// commandCodeAuthPath 是 CLI 的登录文件；测试可替换。
// AuthPath 本机 CLI 登录态路径（测试可替换）。
var AuthPath = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".commandcode", "auth.json")
}

// AuthFile 本机 `commandcode login` 落盘的账号信息。
type AuthFile = ccAuthFile

type ccAuthFile struct {
	APIKey   string `json:"apiKey"`
	UserID   string `json:"userId,omitempty"`
	UserName string `json:"userName,omitempty"`
	KeyName  string `json:"keyName,omitempty"`
}

// ccReadCLIAuth 读取本机 `commandcode login` 的账号（只读，不改文件）
func ccReadCLIAuth() (ccAuthFile, error) {
	path := AuthPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ccAuthFile{}, fmt.Errorf("本机没有 Command Code 登录态（%s）；请先运行 `commandcode login`，或直接粘贴 API Key", path)
		}
		return ccAuthFile{}, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var a ccAuthFile
	if err := json.Unmarshal(b, &a); err != nil {
		return ccAuthFile{}, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	a.APIKey = strings.TrimSpace(a.APIKey)
	if a.APIKey == "" {
		return ccAuthFile{}, fmt.Errorf("%s 里没有 apiKey；请先运行 `commandcode login`", path)
	}
	return a, nil
}

// ── 套餐与模型目录 ────────────────────────────────────────────────────────

// ccPlans 是 CLI 的套餐表（id 前缀 → 展示名），最长前缀优先。
var ccPlans = []struct{ id, name string }{
	{"individual-provider", "Provider"}, {"individual-pro-v1", "Pro"}, {"individual-goat", "GOAT"},
	{"individual-ultra", "Ultra"}, {"individual-max", "Max"}, {"individual-pro", "Pro"},
	{"individual-go", "Go"}, {"teams-pro", "Teams Pro"},
}

func ccPlanName(id string) string {
	id = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(id)), "_", "-")
	for _, p := range ccPlans {
		if strings.HasPrefix(id, p.id) {
			return p.name
		}
	}
	return ""
}

// ccPremium 是 Go / GOAT 这类「只有开放模型」套餐不能用的高级模型。
var ccPremium = map[string]bool{
	"claude-sonnet-5": true, "claude-sonnet-4-6": true, "claude-fable-5-1": true, "claude-fable-5": true,
	"claude-opus-5-5": true, "claude-opus-5": true, "claude-opus-4-8": true, "claude-opus-4-7": true,
	"claude-haiku-4-5-20251001": true, "gpt-5.6-terra": true, "gpt-6-astra": true, "gpt-6.1-sol": true,
	"gpt-6-sol": true, "gpt-5.5": true, "gpt-5.4": true, "gpt-5.3-codex": true, "gpt-5.4-mini": true,
	"google/gemini-3.5-flash": true, "google/gemini-3.1-flash-lite": true, "sakana/fugu-ultra": true,
	"meta/muse-spark-1.1": true,
}

var ccGoBlocked = map[string]bool{
	"meta/muse-spark-1.2": true, "meta/muse-spark-1.3": true, "xai/grok-4.6": true, "xai/grok-4.7": true,
	"xiaomi/mimo-v2.6-pro-ultraspeed": true, "google/gemini-3.7-flash": true, "google/gemini-3.8-flash": true,
	"gpt-5.6-sol": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": true,
}

var ccProBlocked = map[string]bool{
	"claude-fable-5-1": true, "claude-fable-5": true, "claude-opus-5-5": true, "claude-opus-5": true,
	"claude-opus-4-8": true, "claude-opus-4-7": true, "claude-opus-4-6": true, "claude-opus-4-5-20251101": true,
	"gpt-6-astra": true, "gpt-6.1-sol": true, "sakana/fugu-ultra": true,
}

// ccRefuses 判断套餐是否拒绝某模型。未知套餐 / No plan 一律不拦，交给上游
// 以 403 MODEL_NOT_IN_PLAN 拒绝。
func ccRefuses(plan, id string) bool {
	switch plan {
	case "Go":
		return ccPremium[id] || ccGoBlocked[id]
	case "GOAT":
		return ccPremium[id]
	case "Pro":
		return ccProBlocked[id]
	case "Provider":
		return id == "gpt-6.1-sol"
	}
	return false
}

// ccModelInfo 是 CLI 自带模型表（command-code 1.73.0）里 Go 可用的模型；
// 目录拉不到时兜底，并给出模型支持的 reasoning_effort 档位。
type ccModelInfo struct {
	ID      string
	Efforts []string
}

var ccEff5 = []string{"low", "medium", "high", "xhigh", "max"}

var ccGoModels = []ccModelInfo{
	{"gpt-6-luna", ccEff5},
	{"gpt-5.6-luna", ccEff5},
	{"deepseek/deepseek-v4-pro", []string{"high", "max"}},
	{"deepseek/deepseek-v4-flash", []string{"high", "max"}},
	{"deepseek/deepseek-v4-flash-vision-exp", []string{"high", "max"}},
	{"deepseek/deepseek-v4-flash-fast", []string{"low", "high", "max"}},
	{"deepseek/deepseek-v4.1-flash", []string{"low", "high", "max"}},
	{"deepseek/deepseek-v4.1-flash-fast", []string{"low", "high", "max"}},
	{"moonshotai/Kimi-K3", []string{"low", "high", "max"}},
	{"moonshotai/Kimi-K2.7-Code", nil},
	{"moonshotai/Kimi-K2.7-Code-Highspeed", nil},
	{"moonshotai/Kimi-K2.6", nil},
	{"moonshotai/Kimi-K2.5", nil},
	{"z-ai/glm-5.3-flash", []string{"low", "high", "max"}},
	{"z-ai/glm-5.3-flashx", []string{"low", "high", "max"}},
	{"zai-org/GLM-5.3", []string{"low", "high", "max"}},
	{"zai-org/GLM-5.2", []string{"high", "max"}},
	{"zai-org/GLM-5.2-Fast", nil},
	{"zai-org/GLM-5.1", nil},
	{"zai-org/GLM-5", nil},
	{"MiniMaxAI/MiniMax-M3", []string{"low", "medium", "high"}},
	{"MiniMaxAI/MiniMax-M2.7", nil},
	{"MiniMaxAI/MiniMax-M2.5", nil},
	{"xiaomi/mimo-v2.6-pro", nil},
	{"xiaomi/mimo-v2.6-flash", nil},
	{"xiaomi/mimo-v2.5-pro", nil},
	{"xiaomi/mimo-v2.5", nil},
	{"Qwen/Qwen3.8-Omni-Flash", []string{"low", "medium", "xhigh"}},
	{"Qwen/Qwen3.8-Max-0902", []string{"low", "medium", "xhigh"}},
	{"Qwen/Qwen3.8-Max", []string{"low", "medium", "xhigh"}},
	{"Qwen/Qwen3.8-27B", []string{"low", "medium", "xhigh"}},
	{"Qwen/Qwen3.8-Flash", []string{"low", "medium", "xhigh"}},
	{"Qwen/Qwen3.7-Max", nil},
	{"Qwen/Qwen3.7-Plus", nil},
	{"Qwen/Qwen3.7-Flash", nil},
	{"Qwen/Qwen3.6-Max-Preview", nil},
	{"Qwen/Qwen3.6-Plus", nil},
	{"meituan/LongCat-2.0", nil},
	{"stepfun/Step-5-Preview", []string{"low", "medium", "high"}},
	{"stepfun/Step-3.7-Flash", nil},
	{"stepfun/Step-3.5-Flash", nil},
	{"tencent/hy3-paid", nil},
	{"tencent/hy4-preview", []string{"low", "medium", "high"}},
	{"google/gemini-3.6-flash", []string{"low", "medium", "high"}},
	{"google/gemini-3.5-flash-lite", []string{"low", "medium", "high"}},
	{"nvidia/nemotron-3-ultra-550b-a55b", nil},
	{"thinkingmachines/inkling", nil},
	{"thinkingmachines/inkling-small", nil},
	{"stealth/space-bunny-alpha", []string{"low", "medium", "high"}},
	{"stealth/pixel-canary", []string{"low", "medium", "xhigh"}},
	{"poolside/laguna-s-2.1-free", nil},
	{"inclusionai/ling-3.0-flash-free", nil},
	{"inclusionai/ling-3.0-flash-sante:free", nil},
	{"inclusionai/ling-3.1-flash:free", []string{"low", "medium", "high"}},
	{"meta/muse-spark-1.2-contributor", []string{"low", "medium", "high", "xhigh"}},
	{"meta/muse-spark-1.3-contributor", []string{"low", "medium", "high", "xhigh"}},
	{"xai/grok-4.5", []string{"low", "medium", "high"}},
}

func ccGoEfforts() map[string][]string {
	m := make(map[string][]string, len(ccGoModels))
	for _, x := range ccGoModels {
		m[x.ID] = x.Efforts
	}
	return m
}

var ccEffortsByModel = ccGoEfforts()

// ccCatalog 拉取模型目录（Go 的密钥没有 Provider API，目录本身不需要密钥），
// 按套餐过滤后返回。目录拉不到时退回 CLI 自带表。
func (a *Adapter) Catalog(ctx context.Context, p Provider) ([]string, error) {
	cli, err := a.rt.Clients.For(p, 20*time.Second)
	if err != nil {
		return nil, err
	}
	ask := func(withKey bool) []string {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.API()+"/provider/v1/models", nil)
		if withKey {
			if c := ccOf(p); c != nil && c.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+c.APIKey)
			}
		}
		resp, err := cli.Do(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return upstream.ExtractModelIDs(b)
	}
	// Go 的 Key 不被 Provider API 接受，先不带密钥问；非 Go 套餐需要时再带密钥重试。
	ids := ask(false)
	if len(ids) == 0 {
		ids = ask(true)
	}
	if len(ids) == 0 {
		for _, m := range ccGoModels {
			ids = append(ids, m.ID)
		}
	}
	plan := ""
	if c := ccOf(p); c != nil {
		plan = c.Plan
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] || ccRefuses(plan, id) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("Command Code 没有返回可用模型（套餐 %q）", plan)
	}
	return out, nil
}

// ccPlanSnapshot 读取订阅套餐与周期结束时刻。
type ccPlanSnapshot struct {
	ID    string
	Name  string
	Until *time.Time
}

func (a *Adapter) FetchPlan(ctx context.Context, p Provider, key string) (ccPlanSnapshot, error) {
	cli, err := a.rt.Clients.For(p, 30*time.Second)
	if err != nil {
		return ccPlanSnapshot{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.API()+"/alpha/billing/subscriptions", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", commandCodeCLIVersion)
	resp, err := cli.Do(req)
	if err != nil {
		return ccPlanSnapshot{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return ccPlanSnapshot{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, ccErrSnippet(b))
	}
	var r struct {
		Success *bool `json:"success"`
		Data    *struct {
			PlanID           string `json:"planId"`
			Status           string `json:"status"`
			CurrentPeriodEnd any    `json:"currentPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return ccPlanSnapshot{}, err
	}
	if r.Success != nil && !*r.Success {
		return ccPlanSnapshot{}, fmt.Errorf("Command Code 返回 success=false")
	}
	if r.Data == nil || r.Data.PlanID == "" {
		return ccPlanSnapshot{}, fmt.Errorf("订阅响应里没有 planId")
	}
	if r.Data.Status == "canceled" || r.Data.Status == "incomplete_expired" {
		return ccPlanSnapshot{ID: r.Data.PlanID, Name: "No plan"}, nil
	}
	return ccPlanSnapshot{
		ID:    r.Data.PlanID,
		Name:  ccPlanName(r.Data.PlanID),
		Until: ccParseTime(r.Data.CurrentPeriodEnd),
	}, nil
}

// ccWhoami 读取账号标识（/alpha/whoami），失败不阻断导入。
func (a *Adapter) Whoami(ctx context.Context, p Provider, key string) (id, name string) {
	cli, err := a.rt.Clients.For(p, 10*time.Second)
	if err != nil {
		return "", ""
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.API()+"/alpha/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", commandCodeCLIVersion)
	resp, err := cli.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		User struct {
			ID       string `json:"id"`
			UserName string `json:"userName"`
			Email    string `json:"email"`
		} `json:"user"`
	}
	if json.Unmarshal(b, &r) != nil {
		return "", ""
	}
	name = r.User.UserName
	if name == "" {
		name = r.User.Email
	}
	return r.User.ID, name
}

// ccParseTime 解析重置/周期结束时间：unix 秒/毫秒或 RFC3339。
func ccParseTime(v any) *time.Time {
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return &t
		}
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case json.Number:
		f, _ := x.Float64()
		n = f
	}
	if n <= 0 {
		return nil
	}
	if n < 1e12 {
		n *= 1000
	}
	t := time.UnixMilli(int64(n))
	return &t
}

// ── Provider 导入 / 刷新 ──────────────────────────────────────────────────

// ccPersist 创建或原地更新 Command Code Provider，并同步模型（只增不删）。
// providerID 非空时更新该 Provider（用于编辑时替换 Key）；否则按 Key 去重。
// 返回 provider ID 与同步的模型数。
func (a *Adapter) Persist(ctx context.Context, providerID, key, name, proxy string, zdr, fromCLI bool) (string, int, string, error) {
	p := &domain.Provider{
		Kind:    commandCodeKind,
		BaseURL: a.API() + "/alpha/generate",
		Proxy:   proxy,
		CommandCode: &CommandCodeCred{
			APIKey: strings.TrimSpace(key), CLI: fromCLI, ZDR: zdr,
		},
	}
	var old Provider
	var hasOld bool
	if providerID != "" {
		old, hasOld = a.rt.Store.Provider(providerID)
		if hasOld && old.Kind != commandCodeKind {
			hasOld = false
		}
	}
	if !hasOld {
		old, hasOld = a.rt.Store.ProviderByCommandCodeKey(p.CommandCode.APIKey)
	}
	if hasOld {
		p.ID, p.Name = old.ID, old.Name
		if strings.TrimSpace(p.Proxy) == "" {
			p.Proxy = old.Proxy
		}
		if old.CommandCode != nil {
			p.CommandCode.UserID = old.CommandCode.UserID
			p.CommandCode.UserName = old.CommandCode.UserName
			p.CommandCode.Plan = old.CommandCode.Plan
		}
	}
	if strings.TrimSpace(name) != "" {
		p.Name = strings.TrimSpace(name)
	}
	uid, uname := a.Whoami(ctx, *p, p.CommandCode.APIKey)
	if uid != "" {
		p.CommandCode.UserID = uid
	}
	if uname != "" {
		p.CommandCode.UserName = uname
	}
	if p.Name == "" {
		p.Name = "Command Code"
		if uname != "" {
			p.Name += " · " + uname
		}
	}
	if plan, err := a.FetchPlan(ctx, *p, p.CommandCode.APIKey); err == nil {
		p.CommandCode.Plan = plan.Name
	} else if p.CommandCode.Plan == "" {
		// 套餐读不到时仍可导入；模型目录不按套餐过滤，由上游拒绝。
		p.CommandCode.Plan = ""
	}
	now := time.Now()
	p.CommandCode.CheckedAt = &now
	a.rt.Store.UpsertProvider(p)

	models, err := a.Catalog(ctx, *p)
	if err != nil {
		return p.ID, 0, p.CommandCode.Plan, err
	}
	n := 0
	for _, m := range models {
		if !a.rt.Store.HasZedModel(p.ID, m) { // 通用「该 provider 下是否已有同名模型」
			a.rt.Store.UpsertModel(&domain.Model{ProviderID: p.ID, Name: m, Expose: true})
		}
		n++
	}
	return p.ID, n, p.CommandCode.Plan, nil
}

// ccErrSnippet 上游错误原文的短摘要（JSON 优先取 error.message）。
func ccErrSnippet(b []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &v) == nil {
		if v.Error.Message != "" {
			return v.Error.Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	return strings.TrimSpace(string(bytes.TrimSpace(b)))
}

// ccEnvironment CLI 的 process.platform 取值。
func ccEnvironment() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// ReadCLIAuth 读取本机 CLI 登录态。
func ReadCLIAuth() (AuthFile, error) { return ccReadCLIAuth() }

// BuildBody 把 chat 请求体翻译成 CLI envelope（测试与诊断用）。
func BuildBody(raw []byte, model string) ([]byte, string, error) { return ccBuildBody(raw, model) }
