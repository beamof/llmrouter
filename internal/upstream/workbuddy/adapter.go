// Package workbuddy 实现 WorkBuddy / CodeBuddy 上游适配器：复用官方客户端登录态，
// 出站请求对齐官方 CLI 指纹，回程把上游事件流翻译成客户端协议。
package workbuddy

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
	Provider      = domain.Provider
	Model         = domain.Model
	Candidate     = domain.Candidate
	WorkBuddyCred = domain.WorkBuddyCred
	WBUsage       = domain.WBUsage
	Usage         = domain.Usage
	LogEntry      = domain.LogEntry
)

// Adapter WorkBuddy 上游适配器。每个 Provider 的续期锁与目录缓存都归属本实例。
type Adapter struct {
	rt *upstream.Runtime

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	cats  map[string]*wbCatalog
}

// New 创建适配器。
func New(rt *upstream.Runtime) *Adapter {
	return &Adapter{rt: rt, locks: map[string]*sync.Mutex{}, cats: map[string]*wbCatalog{}}
}

// Kinds 接管的 Provider.Kind。
func (a *Adapter) Kinds() []string { return []string{"workbuddy", "workbuddy-intl"} }

// CanServe WorkBuddy 上游只有 chat completions；responses 仅在开启协议转换时承接。
func (a *Adapter) CanServe(protocol domain.Protocol) bool {
	if protocol != domain.ProtocolResponses {
		return true
	}
	return a.rt != nil && a.rt.Store != nil && a.rt.Store.ResponsesConvertEnabled()
}

// ValidateProvider 固定基址并校验凭证（凭证随请求发出，不允许改写基址）。
func (a *Adapter) ValidateProvider(p *domain.Provider) error {
	e, _ := wbOf(*p)
	if e == nil {
		return fmt.Errorf("kind 非法")
	}
	p.BaseURL = e.endpoint + "/v2"
	if p.WB == nil || strings.TrimSpace(p.WB.AccessToken) == "" {
		return fmt.Errorf("WorkBuddy 类型的 provider 必须携带凭证（wb.access_token），编辑时请勿丢弃 wb 字段")
	}
	return nil
}

// Attempt 实现 upstream.Adapter。
func (a *Adapter) Attempt(att *upstream.Attempt) error {
	return a.attempt(att.Writer, att.Request, att.Candidate, att.Body, att.KeyID, att.Protocol == domain.ProtocolResponses, att.Timing, att.Usage)
}

// Probe 用 /v3/config 目录做连通性探测，并核对模型名（顺带刷新目录缓存）。
func (a *Adapter) Probe(ctx context.Context, p domain.Provider, model string) (upstream.ProbeResult, error) {
	e, cred := wbOf(p)
	if e == nil || cred == nil || cred.AccessToken == "" {
		return upstream.ProbeResult{Level: "warn", Message: "未导入 WorkBuddy 凭证，请先在 Provider 弹窗导入"}, nil
	}
	start := time.Now()
	cli, err := a.rt.Clients.For(p, 20*time.Second)
	if err != nil {
		return upstream.ProbeResult{Level: "fail", Message: err.Error()}, nil
	}
	cat, err := wbFetchCatalog(cli, wbEndpoint(p), e, cred)
	res := upstream.ProbeResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Level = "warn"
		res.Message = "目录接口异常（不影响对话接口）: " + err.Error()
		return res, nil
	}
	if len(cat.IDs) > 0 { // 空目录不入缓存，否则会盖掉可用的兜底清单
		a.mu.Lock()
		if a.cats == nil {
			a.cats = map[string]*wbCatalog{}
		}
		a.cats[p.ID] = cat
		a.mu.Unlock()
	}
	if len(cat.IDs) == 0 {
		res.Level = "warn"
		res.Message = "端口通，但目录为空（可能无权访问模型清单）"
		return res, nil
	}
	res.Level = "ok"
	res.Message = fmt.Sprintf("端口通，目录共 %d 个模型", len(cat.IDs))
	for _, id := range cat.IDs {
		if strings.EqualFold(id, model) {
			return res, nil
		}
	}
	res.Level = "warn"
	res.Message = fmt.Sprintf("端口通，但目录（%d 个）中没有 %q，请核对模型名", len(cat.IDs), model)
	return res, nil
}

// Forget 丢弃该 Provider 的续期锁与目录缓存（删除 Provider 时调用）。
func (a *Adapter) Forget(id string) {
	a.forgetProvider(id)
}

// InvalidateCatalog 让缓存失效，下次请求重新拉取（刷新/导入凭证后调用）。
func (a *Adapter) InvalidateCatalog(providerID string) { a.invalidateCatalog(providerID) }

// Models 返回该 Provider 的模型目录（缓存或内置兜底），给后台下拉/模型同步用。
func (a *Adapter) Models(p domain.Provider) []string {
	ids := a.catalogFor(p).IDs
	return append([]string(nil), ids...)
}

// Checkin 手动签到。
func (a *Adapter) Checkin(p domain.Provider) (already bool, err error) { return a.checkin(p) }

// UserResource 读取余额快照。
func (a *Adapter) UserResource(p domain.Provider) (*domain.WBUsage, error) { return a.userResource(p) }

// CheckinLoop 每日签到循环。
func (a *Adapter) CheckinLoop() { a.checkinLoop() }

// CheckinRound 执行一轮签到/余额刷新（测试可直接调用）。
func (a *Adapter) CheckinRound(now time.Time) { a.checkinRound(now) }

// CheckinDay 签到日口径（北京时间 2006-01-02），供测试与诊断。
func CheckinDay(now time.Time) string { return checkinDay(now) }

// Edition 地区身份（对外只读视图）。
type Edition = wbEdition

// ScanHit 本机扫描命中的一条登录态。
type ScanHit = wbScanHit

// CN / Intl 两个地区身份。
var (
	CN   = &wbCN
	Intl = &wbIntl
)

// EditionByKind 按 Kind 取地区身份。
func EditionByKind(kind string) (*Edition, bool) {
	switch kind {
	case "workbuddy":
		return &wbCN, true
	case "workbuddy-intl":
		return &wbIntl, true
	}
	return nil, false
}

// Intl 是否国际版。
func (e *Edition) Intl() bool { return e != nil && e.intl }

// Endpoint 控制面端点。
func (e *Edition) Endpoint() string { return e.endpoint }

// Label 展示名。
func (e *Edition) Label() string { return e.label }

// Of 取地区身份与凭证（非 WorkBuddy provider 返回 nil）。
func Of(p domain.Provider) (*Edition, *domain.WorkBuddyCred) { return wbOf(p) }

// Scan 扫描本机客户端登录态快照。
func Scan(secret string) []*ScanHit { return wbScan(secret) }

// ParseInfoBytes 解析粘贴的 .info 形态 JSON。
func ParseInfoBytes(raw []byte, secret string) *ScanHit { return wbParseInfoBytes(raw, secret) }

// RefreshToken 手动续期。
func (a *Adapter) RefreshToken(p domain.Provider, force bool) (*domain.WorkBuddyCred, error) {
	return a.refreshToken(p, force)
}

// CatalogSummary 返回目录模型数与是否为远端拉取。
func (a *Adapter) CatalogSummary(p domain.Provider) (int, bool) {
	cat := a.catalogFor(p)
	return len(cat.IDs), cat.Remote
}

// 测试与诊断用的导出常量/工具（避免外部测试复制上游契约细节）。
const (
	// CatalogBody 官方 /v3/config 目录响应样例
	CatalogBody = `{"code":0,"data":{
	"agents":[{"name":"desktop","models":["completion-legacy"]},{"name":"cli","models":["GLM-5.0","deepseek-v4-flash","blocked-model","ghost-model"]}],
	"availableModels":["GLM-5.0","deepseek-v4-flash","blocked-model","future-x"],
	"models":[
	{"id":"GLM-5.0","reasoning":{"supportedEfforts":["low","medium","high"],"defaultEffort":"medium"},"maxOutputTokens":48000},
	{"id":"deepseek-v4-flash","reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"},"maxOutputTokens":50000},
	{"id":"completion-legacy","maxOutputTokens":48000},
	{"id":"tiny-model","maxOutputTokens":8000},
	{"id":"extra-model","maxOutputTokens":48000,"supportsExtra":true},
	{"id":"blocked-model","maxOutputTokens":48000,"disabled":true}
]}}`
	// CLIVersion 官方 CLI 版本
	CLIVersion = wbCLIVersion
	// DefaultSystemPrompt 官方 CLI 默认系统提示词
	DefaultSystemPrompt = wbDefaultSystemPrompt
)

// AAD 加密信封的附加认证数据。
func AAD(keyID string, suite int) []byte { return wbAAD(keyID, suite) }

// TimePtr 取时间指针。
func TimePtr(t time.Time) *time.Time { return wbTimePtr(t) }

// CLIUserAgent 官方 CLI User-Agent。
func CLIUserAgent() string { return wbCLIUserAgent() }

// UTC8 东八区时区。
var UTC8 = wbUTC8

// AuthDir 本机客户端登录态目录。
func AuthDir() string { return wbAuthDir() }

// ListModels 返回该 Provider 的模型目录（缓存或内置兜底）。
func (a *Adapter) ListModels(_ context.Context, p domain.Provider) ([]string, error) {
	if _, cred := wbOf(p); cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("该 provider 未导入 WorkBuddy 凭证")
	}
	return a.Models(p), nil
}
