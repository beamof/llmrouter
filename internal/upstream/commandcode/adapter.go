// Package commandcode 实现 Command Code 订阅（commandcode.ai）适配器。
// 数据面走 CLI 线协议：POST /alpha/generate，回程为逐行 JSON 事件流。
package commandcode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
)

// Kind Provider.Kind 取值。
const (
	// Kind Provider.Kind 取值。
	Kind = "commandcode"
	// DefaultAPI 官方数据面基址。
	DefaultAPI = "https://api.commandcode.ai"
)

// 本包内部沿用领域模型的短名称。
type (
	Provider        = domain.Provider
	Model           = domain.Model
	CommandCodeCred = domain.CommandCodeCred
	Candidate       = domain.Candidate
	LogEntry        = domain.LogEntry
)

// Adapter Command Code 上游适配器。
type Adapter struct {
	rt   *upstream.Runtime
	base string // 数据面基址覆盖（测试注入；空 = 官方端点）
}

// New 创建适配器。base 为空时使用官方端点。
func New(rt *upstream.Runtime, base string) *Adapter {
	return &Adapter{rt: rt, base: base}
}

// Kinds 接管的 Provider.Kind。
func (a *Adapter) Kinds() []string { return []string{Kind} }

// CanServe Command Code 适配层两种接入协议都直接承接。
func (a *Adapter) CanServe(domain.Protocol) bool { return true }

// ValidateProvider 固定数据面基址并校验 API Key（避免把密钥发往任意主机）。
func (a *Adapter) ValidateProvider(p *domain.Provider) error {
	if p.CommandCode == nil || strings.TrimSpace(p.CommandCode.APIKey) == "" {
		return fmt.Errorf("Command Code 类型的 provider 必须携带凭证（commandcode.api_key），编辑时请勿丢弃 commandcode 字段")
	}
	p.BaseURL = a.API() + "/alpha/generate"
	p.ResponsesURL = ""
	return nil
}

// Cred 取 Command Code 凭证（Kind 不匹配时返回 nil）。
func Cred(p domain.Provider) *domain.CommandCodeCred {
	if p.Kind == Kind {
		return p.CommandCode
	}
	return nil
}

// API 数据面基址（测试可注入 base）。
func (a *Adapter) API() string {
	if a.base != "" {
		return a.base
	}
	return commandCodeAPI
}

// PersistResult 导入/刷新后的摘要。
type PersistResult struct {
	ProviderID string
	Models     int
	Plan       string
}

// Persist 创建或原地更新 Provider，并同步模型（只增不删）。
func (a *Adapter) PersistProvider(ctx context.Context, providerID, key, name, proxy string, zdr, fromCLI bool) (PersistResult, error) {
	id, n, plan, err := a.Persist(ctx, providerID, key, name, proxy, zdr, fromCLI)
	return PersistResult{ProviderID: id, Models: n, Plan: plan}, err
}

// Probe 用模型目录做连通性探测（不消耗 token）。
func (a *Adapter) Probe(ctx context.Context, p domain.Provider, model string) (upstream.ProbeResult, error) {
	models, err := a.Catalog(ctx, p)
	if err != nil {
		return upstream.ProbeResult{Level: "fail", Message: "连接失败: " + err.Error()}, nil
	}
	res := upstream.ProbeResult{Level: "ok", Message: fmt.Sprintf("端口通，模型名在 Command Code 目录中（共 %d 个）", len(models))}
	if !contains(models, model) {
		res.Level = "warn"
		res.Message = fmt.Sprintf("端口通，但 Command Code 目录（%d 个）中没有 %q，请核对模型名", len(models), model)
	}
	return res, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Refresh 重新读取套餐并同步模型；CLI 导入的账号会重新读本机 auth.json。
func (a *Adapter) Refresh(ctx context.Context, providerID string) (string, int, error) {
	p, ok := a.rt.Store.Provider(providerID)
	if !ok {
		return "", 0, fmt.Errorf("provider 不存在")
	}
	cred := Cred(p)
	if cred == nil {
		return "", 0, fmt.Errorf("该 provider 不是 Command Code 类型")
	}
	key := cred.APIKey
	if cred.CLI {
		if af, err := ccReadCLIAuth(); err == nil {
			key = af.APIKey
			if af.UserID != "" {
				cred.UserID = af.UserID
			}
			if af.UserName != "" {
				cred.UserName = af.UserName
			}
		}
	}
	updated := *cred
	updated.APIKey = key
	if plan, err := a.FetchPlan(ctx, p, key); err == nil {
		updated.Plan = plan.Name
	}
	now := time.Now()
	updated.CheckedAt = &now
	a.rt.Store.UpdateCommandCodeAuth(p.ID, &updated)
	p.CommandCode = &updated
	models, err := a.Catalog(ctx, p)
	if err != nil {
		return updated.Plan, 0, fmt.Errorf("拉取 Command Code 模型目录失败: %w", err)
	}
	n := 0
	for _, m := range models {
		if !a.rt.Store.HasZedModel(p.ID, m) {
			a.rt.Store.UpsertModel(&domain.Model{ProviderID: p.ID, Name: m, Expose: true})
		}
		n++
	}
	return updated.Plan, n, nil
}

// ListModels 拉取 Command Code 模型目录（按套餐过滤）。
func (a *Adapter) ListModels(ctx context.Context, p domain.Provider) ([]string, error) {
	return a.Catalog(ctx, p)
}
