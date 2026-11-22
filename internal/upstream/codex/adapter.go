// Package codex 实现 Codex 套餐（Sign in with ChatGPT 的 ChatGPT plan usage）
// 上游适配器：OAuth PKCE 登录、token 续期、模型目录同步与 Responses 线协议适配。
package codex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
)

// 本包内部沿用领域模型的短名称。
type (
	Provider  = domain.Provider
	Model     = domain.Model
	Candidate = domain.Candidate
	CodexCred = domain.CodexCred
	Usage     = domain.Usage
	LogEntry  = domain.LogEntry
)

// Adapter Codex 套餐上游适配器。
type Adapter struct {
	rt *upstream.Runtime

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	loginMu sync.Mutex
	logins  map[string]*loginSession

	jwks jwksCache

	authBase string // 授权服务器基址覆盖（测试注入；空 = 官方端点）
	apiBase  string // 推理 API 基址覆盖（测试注入；空 = 官方端点）
}

// New 创建适配器。
func New(rt *upstream.Runtime) *Adapter {
	return &Adapter{rt: rt, locks: map[string]*sync.Mutex{}, logins: map[string]*loginSession{}}
}

// NewWithBases 创建适配器并覆盖授权 / 推理基址（测试注入用）。
func NewWithBases(rt *upstream.Runtime, authBase, apiBase string) *Adapter {
	a := New(rt)
	a.authBase, a.apiBase = authBase, apiBase
	return a
}

// Kinds 接管的 Provider.Kind。
func (a *Adapter) Kinds() []string { return []string{"codex"} }

// CanServe 适配层两种接入协议都承接（chat 出站转 Responses）。
func (a *Adapter) CanServe(domain.Protocol) bool { return true }

// ValidateProvider 固定数据面基址并校验凭证。
func (a *Adapter) ValidateProvider(p *domain.Provider) error {
	if p.Codex == nil || strings.TrimSpace(p.Codex.Subject) == "" {
		return fmt.Errorf("Codex 类型的 provider 必须携带凭证（codex.subject），编辑时请勿丢弃 codex 字段")
	}
	p.BaseURL = a.api() + codexResponsesPath
	return nil
}

// Attempt 实现 upstream.Adapter（见 codexwire.go）。
func (a *Adapter) Attempt(att *upstream.Attempt) error {
	return a.attempt(att.Writer, att.Request, att.Candidate, att.Body, att.KeyID,
		att.Protocol == domain.ProtocolResponses, att.Timing, att.Usage)
}

// Probe 用模型目录做连通性探测，并核对模型名。
func (a *Adapter) Probe(ctx context.Context, p domain.Provider, model string) (upstream.ProbeResult, error) {
	start := time.Now()
	models, err := a.Catalog(ctx, p)
	res := upstream.ProbeResult{LatencyMS: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		res.Level = "fail"
		res.Message = "连接失败: " + err.Error()
	default:
		res.Level = "ok"
		res.Message = fmt.Sprintf("端口通，模型名在账号目录中（共 %d 个）", len(models))
		if !contains(models, model) {
			res.Level = "warn"
			res.Message = fmt.Sprintf("端口通，但账号目录（%d 个）中没有 %q，请核对模型名", len(models), model)
		}
	}
	return res, nil
}

// ListModels 拉取账号可见的模型目录（后台「添加模型」下拉用）。
func (a *Adapter) ListModels(ctx context.Context, p domain.Provider) ([]string, error) {
	return a.Catalog(ctx, p)
}

// Catalog 确保凭证新鲜后拉取模型目录。
func (a *Adapter) Catalog(ctx context.Context, p Provider) ([]string, error) {
	cred, err := a.refreshToken(p, false)
	if err != nil {
		return nil, err
	}
	cli, cerr := a.rt.Clients.For(p, 15*time.Second)
	if cerr != nil {
		return nil, cerr
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return a.fetchModels(cctx, cli, cred)
}

// Cred 取 Codex 凭证（Kind 不匹配时返回 nil）。
func Cred(p Provider) *CodexCred {
	if p.Kind == "codex" {
		return p.Codex
	}
	return nil
}

// Forget 释放该 Provider 的续期单飞锁，并尽力吊销远端可续期会话。
// 吊销失败只记日志：用户也可以在 ChatGPT 设置 → 应用里手动断开。
func (a *Adapter) Forget(id string) {
	a.mu.Lock()
	delete(a.locks, id)
	a.mu.Unlock()
	p, ok := a.rt.Store.Provider(id)
	if !ok {
		return
	}
	cred := codexOf(p)
	if cred == nil || cred.RefreshToken == "" {
		return
	}
	cli, err := a.rt.Clients.For(p, 10*time.Second)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.revoke(ctx, cli, cred.ClientID, cred.RefreshToken); err != nil {
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, ProviderID: id,
			Message: "Codex 套餐会话吊销失败（可在 ChatGPT 设置里手动断开）", Detail: err.Error()})
		return
	}
	a.rt.Logf(LogEntry{Level: "info", Kind: domain.KindUpstream, ProviderID: id,
		Message: "Codex 套餐会话已吊销（Provider 删除）"})
}

// ── token 续期（懒续期 + 401 强制续期共用） ───────────────────────────────

// codexProviderLock 续期单飞锁：同一 provider 的并发续期只发一次上游请求
func (a *Adapter) providerLock(id string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.locks == nil {
		a.locks = map[string]*sync.Mutex{}
	}
	l, ok := a.locks[id]
	if !ok {
		l = &sync.Mutex{}
		a.locks[id] = l
	}
	return l
}

// codexTokenExpiring access token 是否临期（过期时刻未知时视为临期）
func codexTokenExpiring(c *CodexCred) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	return time.Until(c.Expires()) < codexRefreshWindow
}

// refreshToken 续期 access token 并回写。force 时无视临期窗口（401 后强制换新）。
// refresh token 轮换：每次成功续期都一并保存新下发的 refresh token。
func (a *Adapter) refreshToken(p Provider, force bool) (*CodexCred, error) {
	lock := a.providerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	cur, ok := a.rt.Store.Provider(p.ID)
	if !ok {
		return nil, fmt.Errorf("provider 已删除")
	}
	cred := codexOf(cur)
	if cred == nil {
		return nil, fmt.Errorf("provider 缺少 Codex 凭证")
	}
	if !force && !codexTokenExpiring(cred) {
		return cred, nil
	}
	if strings.TrimSpace(cred.RefreshToken) == "" || strings.TrimSpace(cred.ClientID) == "" {
		return nil, fmt.Errorf("凭证缺少 refresh token / client_id，请重新登录 ChatGPT 账号")
	}
	cli, err := a.rt.Clients.For(cur, 20*time.Second)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tr, err := a.refreshTokens(ctx, cli, cred.ClientID, cred.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("续期 Codex 套餐凭证: %v", err)
	}
	nc := a.credFromToken(cred, tr)
	if !a.rt.Store.UpdateCodexAuth(p.ID, nc) {
		return nil, fmt.Errorf("续期结果未能落盘（provider 可能已删除）")
	}
	return nc, nil
}

// credFromToken 把 token 端点响应并进现有凭证（保留身份与 client_id 等不变字段）。
// refresh token 轮换时才覆盖保存值。
func (a *Adapter) credFromToken(cred *CodexCred, tr *tokenResponse) *CodexCred {
	nc := *cred
	nc.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		nc.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		nc.IDToken = tr.IDToken // 留作下次登录的 id_token_hint
	}
	exp := a.rt.NowTime().Add(time.Duration(tr.ExpiresIn) * time.Second)
	nc.ExpiresAt = &exp
	if tr.ExpiresIn > 0 {
		// refresh token 30 天滚动：以本次续期时刻起算
		rexp := a.rt.NowTime().Add(30 * 24 * time.Hour)
		nc.RefreshExpiresAt = &rexp
	}
	if tr.Scope != "" {
		nc.Scopes = strings.Fields(tr.Scope)
		nc.PlanUsage = false
		for _, s := range nc.Scopes {
			if s == codexPlanScope {
				nc.PlanUsage = true
			}
		}
	}
	now := a.rt.NowTime()
	nc.CheckedAt = &now
	return &nc
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
