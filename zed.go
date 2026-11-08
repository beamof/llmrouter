package main

// ─────────────────── Zed 上游：常量、凭证操作与账号 API ───────────────────
//
// Zed 上游不是公开 API：请求必须伪装成原生 Zed 客户端（User-Agent / x-zed-version /
// 专用请求头），数据面恒为流式，且外层包一层 Zed 自有信封（见 zedwire.go）。
// 账号接入走 zed.dev 的"原生应用登录"（见 zedlogin.go）：回环端口 + 临时 RSA 密钥，
// 与 OAuth code/PKCE 无关。线协议细节参照 ccLoad（MIT）逆向实现。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	zedSignInURL      = "https://zed.dev/native_app_signin"
	zedCloudBase      = "https://cloud.zed.dev"
	zedCompletionsURL = zedCloudBase + "/completions"
	zedVersion        = "1.23.2" // 伪装的 Zed 客户端版本，随上游演进更新

	// Zed 目录中的 provider 名（信封 provider 字段用）
	zedProviderOpenAI    = "open_ai"   // gpt-*，线协议 = OpenAI Responses
	zedProviderAnthropic = "anthropic" // claude-*，线协议 = Anthropic Messages

	// zedRefreshWindow JWT 临期窗口：距过期不足 2 分钟时提前重铸
	zedRefreshWindow = 2 * time.Minute
)

// zedSystemIDEnv 覆盖本机 Zed 安装标识的环境变量（表单值优先于它）
const zedSystemIDEnv = "LLMROUTER_ZED_SYSTEM_ID"

// zedOf 返回 provider 的 Zed 凭证；非 Zed provider 返回 nil
func zedOf(p Provider) *ZedCred {
	if p.Kind == "zed" {
		return p.Zed
	}
	return nil
}

// zedProviderForModel 按模型名前缀选 Zed provider。Zed 目录的前缀互不重叠，
// 因此不必在每个模型条目旁再存一份 provider 字段。
func zedProviderForModel(model string) (string, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "gpt-"):
		return zedProviderOpenAI, true
	case strings.HasPrefix(model, "claude-"):
		return zedProviderAnthropic, true
	}
	return "", false
}

// zedUserAgent 原生 Zed 客户端身份（对齐 Zed 自身的 UA 形态 "Zed/1.23.2 (macos; aarch64)"）
func zedUserAgent() string {
	os := runtime.GOOS
	if os == "darwin" {
		os = "macos"
	}
	arch := map[string]string{
		"386": "x86", "amd64": "x86_64", "arm64": "aarch64", "loong64": "loongarch64",
	}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return fmt.Sprintf("Zed/%s (%s; %s)", zedVersion, os, arch)
}

// nativeAuthorization 账号类端点（llm_tokens / users/me）的鉴权值：
// "<user_id> <原生凭据 JSON>"（Zed 私有格式，原生凭据作为整体参与鉴权）
func (c *ZedCred) nativeAuthorization() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.UserID) + " " + strings.TrimSpace(string(c.NativeCredential))
}

// tokenExpiring JWT 是否临期（过期时刻未知时视为临期，逼一次重铸拿到明确时刻）
func (c *ZedCred) tokenExpiring() bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	return time.Until(c.expires()) < zedRefreshWindow
}

// zedJWTExpiry 解码 Zed LLM token（JWT）的 exp 声明，得到过期时刻
func zedJWTExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("zed LLM token 不是 JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("zed LLM token payload 不是合法 base64url")
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, errors.New("zed LLM token 缺少 exp 声明")
	}
	return time.Unix(claims.Exp, 0), nil
}

// ── 账号 API（base 可注入，测试指向 httptest） ─────────────────────────────

// zedAPICall 账号类请求的公共骨架：固定原生客户端头，systemID 非空时带安装标识
func zedAPICall(ctx context.Context, cli *http.Client, method, url, authorization, systemID string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", zedUserAgent())
	if systemID != "" {
		req.Header.Set("x-zed-system-id", systemID)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// zedMintLLMToken 用原生凭据换取短期推理 JWT，返回更新了 token 字段的新凭证。
// 原生凭据长期有效，重铸不需要刷新令牌（这点与 WorkBuddy 的一次性轮换不同）。
func zedMintLLMToken(ctx context.Context, cli *http.Client, base string, cred *ZedCred) (*ZedCred, error) {
	if cred == nil || len(cred.NativeCredential) == 0 {
		return nil, errors.New("zed 凭证缺少原生凭据")
	}
	// 端点要求 body 为 {"organization_id": null}（个人账号；organization_id 传 UUID
	// 时代表组织账号）。实测空 body 与 "{}" 都会被上游以
	// "Failed to read request body: Invalid JSON" 400 拒绝（对齐 zed-free 抓包）。
	body, status, err := zedAPICall(ctx, cli, http.MethodPost, strings.TrimRight(base, "/")+"/client/llm_tokens",
		cred.nativeAuthorization(), cred.SystemID, []byte(`{"organization_id":null}`))
	if err != nil {
		return nil, fmt.Errorf("重铸请求失败: %v", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("重铸返回 HTTP %d: %s", status, errSnippet(body))
	}
	var v struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &v); err != nil || strings.TrimSpace(v.Token) == "" {
		return nil, fmt.Errorf("重铸响应缺少 token（HTTP %d）", status)
	}
	expiry, err := zedJWTExpiry(v.Token)
	if err != nil {
		return nil, fmt.Errorf("重铸响应 token 无效: %v", err)
	}
	nc := *cred
	nc.AccessToken = v.Token
	nc.ExpiresAt = &expiry
	return &nc, nil
}

// zedFetchModels 拉取模型目录，只保留本网关能转换的条目（gpt-*/claude-*）。
func zedFetchModels(ctx context.Context, cli *http.Client, base string, cred *ZedCred) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", zedUserAgent())
	req.Header.Set("x-zed-version", zedVersion)
	if cred.SystemID != "" {
		req.Header.Set("x-zed-system-id", cred.SystemID)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("模型目录请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("模型目录返回 HTTP %d: %s", resp.StatusCode, errSnippet(b))
	}
	var v struct {
		Models []struct {
			ID         string `json:"id"`
			Provider   string `json:"provider"`
			IsDisabled bool   `json:"is_disabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("模型目录不是合法 JSON")
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range v.Models {
		id := strings.TrimSpace(m.ID)
		provider, ok := zedProviderForModel(id)
		if id == "" || m.IsDisabled || !ok || m.Provider != provider {
			continue // 目录里的 provider 与前缀推导不一致 = 不认识的线协议，跳过
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("模型目录里没有本网关支持的模型（gpt-*/claude-*）")
	}
	return out, nil
}

// zedAccount /client/users/me 的账号与套餐摘要
type zedAccount struct {
	Username     string
	GitHubLogin  string
	PlanType     string
	Used         *int64
	Limit        *int64
	Subscription string
}

// zedFetchAccount 读取账号身份与套餐用量（登录与手动刷新时调用）
func zedFetchAccount(ctx context.Context, cli *http.Client, base string, cred *ZedCred) (*zedAccount, error) {
	body, status, err := zedAPICall(ctx, cli, http.MethodGet, strings.TrimRight(base, "/")+"/client/users/me",
		cred.nativeAuthorization(), cred.SystemID, nil)
	if err != nil {
		return nil, fmt.Errorf("账号信息请求失败: %v", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("账号信息返回 HTTP %d: %s", status, errSnippet(body))
	}
	var v struct {
		User struct {
			Username    string `json:"username"`
			GitHubLogin string `json:"github_login"`
		} `json:"user"`
		Plan struct {
			PlanV3             string `json:"plan_v3"`
			PlanV2             string `json:"plan_v2"`
			Plan               string `json:"plan"`
			SubscriptionPeriod struct {
				EndedAt string `json:"ended_at"`
			} `json:"subscription_period"`
			Usage struct {
				ModelRequests struct {
					Used  *int64          `json:"used"`
					Limit json.RawMessage `json:"limit"` // 数字或 {limited:n} 或 null
				} `json:"model_requests"`
			} `json:"usage"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("账号信息不是合法 JSON")
	}
	plan := v.Plan.PlanV3
	if plan == "" {
		plan = v.Plan.PlanV2
	}
	if plan == "" {
		plan = v.Plan.Plan
	}
	return &zedAccount{
		Username:     strings.TrimSpace(v.User.Username),
		GitHubLogin:  strings.TrimSpace(v.User.GitHubLogin),
		PlanType:     plan,
		Used:         v.Plan.Usage.ModelRequests.Used,
		Limit:        zedUsageLimit(v.Plan.Usage.ModelRequests.Limit),
		Subscription: strings.TrimSpace(v.Plan.SubscriptionPeriod.EndedAt),
	}, nil
}

// zedUsageLimit 兼容 limit 的三种形态：数字 / {limited:n} / null
func zedUsageLimit(raw json.RawMessage) *int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return &n
	}
	var obj struct {
		Limited *int64 `json:"limited"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Limited
	}
	return nil
}

// zedPlanSnapshot 账号摘要 → 套餐快照（写回凭证）
func (a *zedAccount) zedPlanSnapshot() *ZedPlan {
	return &ZedPlan{
		PlanType: a.PlanType, Used: a.Used, Limit: a.Limit,
		SubscriptionEnd: a.Subscription, CheckedAt: time.Now(),
	}
}

// ── JWT 重铸（懒续期 + 401 强制重铸共用） ─────────────────────────────────

// zedProviderLock 重铸单飞锁：同一 provider 的并发重铸只发一次上游请求
func (s *Server) zedProviderLock(id string) *sync.Mutex {
	s.zedMu.Lock()
	defer s.zedMu.Unlock()
	if s.zedLocks == nil {
		s.zedLocks = map[string]*sync.Mutex{}
	}
	l, ok := s.zedLocks[id]
	if !ok {
		l = &sync.Mutex{}
		s.zedLocks[id] = l
	}
	return l
}

// zedForgetProvider 丢弃该 provider 的重铸锁（删除 Provider 时调用）
func (s *Server) zedForgetProvider(id string) {
	s.zedMu.Lock()
	delete(s.zedLocks, id)
	s.zedMu.Unlock()
}

// zedCloudBase 当前 Zed 云端基址（测试可注入，默认官方端点）
func (s *Server) zedCloud() string {
	if s.zedBase != "" {
		return s.zedBase
	}
	return zedCloudBase
}

// zedRefreshToken 重铸 Zed 短期 JWT 并回写。force 时无视临期窗口（401/403 后强制换新）。
// 原生凭据长期有效，不存在"刷新令牌失效"，重铸失败通常意味着原生凭据被吊销。
func (s *Server) zedRefreshToken(p Provider, force bool) (*ZedCred, error) {
	lock := s.zedProviderLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	cur, ok := s.store.Provider(p.ID)
	if !ok {
		return nil, fmt.Errorf("provider 已删除")
	}
	cred := zedOf(cur)
	if cred == nil {
		return nil, fmt.Errorf("provider 缺少 Zed 凭证")
	}
	if !force && !cred.tokenExpiring() {
		return cred, nil
	}
	cli, err := s.upstreamClient(cur, 20*time.Second)
	if err != nil {
		return nil, err
	}
	nc, err := zedMintLLMToken(context.Background(), cli, s.zedCloud(), cred)
	if err != nil {
		return nil, fmt.Errorf("重铸 Zed JWT: %v", err)
	}
	if !s.store.UpdateZedAuth(p.ID, nc) {
		return nil, fmt.Errorf("重铸结果未能落盘（provider 可能已删除）")
	}
	return nc, nil
}

// ── 错误分类 ───────────────────────────────────────────────────────────────

// zedCredentialRejected 凭据级拒绝（401，或不含 "plan" 的 403）：
// 强制重铸 JWT 后原候选重试一次。
func zedCredentialRejected(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	return status == http.StatusForbidden && !zedModelPlanRejected(status, body)
}

// zedModelPlanRejected 套餐级拒绝（403 且响应含 "plan"）：当前模型不在套餐内，
// 只静音该模型条目，渠道内其他模型继续可用。
func zedModelPlanRejected(status int, body []byte) bool {
	return status == http.StatusForbidden && bytes.Contains(bytes.ToLower(body), []byte("plan"))
}

// zedPlanMute 套餐拒绝的静音时长：静默 1 小时。套餐变化（升级）后可手动解除。
const zedPlanMute = time.Hour
