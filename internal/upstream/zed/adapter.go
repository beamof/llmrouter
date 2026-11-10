// Package zed 实现 Zed 账号上游适配器：原生登录、JWT 重铸、
// gpt→Responses / claude→Anthropic 线协议转换与回程翻译。
package zed

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
	ZedCred   = domain.ZedCred
	ZedPlan   = domain.ZedPlan
	Usage     = domain.Usage
	LogEntry  = domain.LogEntry
)

// Adapter Zed 上游适配器。
type Adapter struct {
	rt *upstream.Runtime

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	loginMu sync.Mutex
	logins  map[string]*zedLoginSession

	base string // 云端基址覆盖（测试注入；空 = 官方端点）
}

// New 创建适配器。
func New(rt *upstream.Runtime, base string) *Adapter {
	return &Adapter{rt: rt, locks: map[string]*sync.Mutex{}, logins: map[string]*zedLoginSession{}, base: base}
}

// Kinds 接管的 Provider.Kind。
func (a *Adapter) Kinds() []string { return []string{"zed"} }

// CanServe Zed 适配层两种接入协议都直接承接。
func (a *Adapter) CanServe(domain.Protocol) bool { return true }

// ValidateProvider 固定数据面基址并校验凭证。
func (a *Adapter) ValidateProvider(p *domain.Provider) error {
	if p.Zed == nil || strings.TrimSpace(p.Zed.UserID) == "" {
		return fmt.Errorf("Zed 类型的 provider 必须携带凭证（zed.user_id），编辑时请勿丢弃 zed 字段")
	}
	p.BaseURL = a.cloud() + "/completions"
	return nil
}

// Attempt 实现 upstream.Adapter。
func (a *Adapter) Attempt(att *upstream.Attempt) error {
	return a.attempt(att.Writer, att.Request, att.Candidate, att.Body, att.KeyID, att.Protocol == domain.ProtocolResponses, att.Timing, att.Usage)
}

// Probe 用云端模型目录做连通性探测，并核对模型名。
func (a *Adapter) Probe(ctx context.Context, p domain.Provider, model string) (upstream.ProbeResult, error) {
	start := time.Now()
	models, err := a.catalog(p)
	res := upstream.ProbeResult{LatencyMS: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		res.Level = "fail"
		res.Message = "连接失败: " + err.Error()
	default:
		res.Level = "ok"
		res.Message = fmt.Sprintf("端口通，模型名在 Zed 目录中（共 %d 个）", len(models))
		if !contains(models, model) {
			res.Level = "warn"
			res.Message = fmt.Sprintf("端口通，但 Zed 目录（%d 个）中没有 %q，请核对模型名", len(models), model)
		}
	}
	return res, nil
}

// Cloud 云端基址。
func (a *Adapter) Cloud() string { return a.cloud() }

// Catalog 拉取模型目录。
func (a *Adapter) Catalog(p domain.Provider) ([]string, error) { return a.catalog(p) }

// RefreshToken 重铸推理 JWT。
func (a *Adapter) RefreshToken(p domain.Provider, force bool) (*domain.ZedCred, error) {
	return a.refreshToken(p, force)
}

// Forget 丢弃该 Provider 的重铸锁（删除 Provider 时调用）。
func (a *Adapter) Forget(id string) { a.forgetProvider(id) }

// Cred 取 Zed 凭证（Kind 不匹配时返回 nil）。
func Cred(p domain.Provider) *domain.ZedCred {
	if p.Kind == "zed" {
		return p.Zed
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ListModels 拉取 Zed 云端模型目录。
func (a *Adapter) ListModels(_ context.Context, p domain.Provider) ([]string, error) {
	return a.catalog(p)
}
