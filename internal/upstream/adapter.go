// Package upstream 定义上游适配器的统一契约、注册表与共享运行时。
//
// 标准 OpenAI 兼容上游使用 standard 适配器；私有协议上游（WorkBuddy、Zed、
// Command Code）各自实现 Adapter 并在 Registry 注册。网关的候选循环只面向
// Adapter，不再感知具体厂商。
package upstream

import (
	"context"
	"net/http"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/store"
)

// Attempt 一次候选尝试所需的全部输入。
type Attempt struct {
	Writer    http.ResponseWriter
	Request   *http.Request
	Candidate domain.Candidate
	Body      []byte // 已改写为目标模型名的请求体
	KeyID     string
	Protocol  domain.Protocol
	Timing    *Timing
	Usage     *domain.CallSample
}

// Adapter 一类上游的完整能力描述。
type Adapter interface {
	// Kinds 该适配器接管的 Provider.Kind 值；标准 OpenAI 兼容上游为空串。
	Kinds() []string
	// CanServe 该适配器能否承接客户端接入协议。
	CanServe(protocol domain.Protocol) bool
	// ValidateProvider 校验并规范化 Provider（固定基址、必备凭证等）。
	ValidateProvider(p *domain.Provider) error
	// Attempt 处理一个候选的完整生命周期。成功时已向客户端写出响应；
	// 失败时返回错误，由候选循环换下一个候选。
	Attempt(a *Attempt) error
	// Probe 连通性探测（后台「测试」按钮）。
	Probe(ctx context.Context, p domain.Provider, model string) (ProbeResult, error)
	// ListModels 拉取该上游的模型目录（后台模型下拉/同步用）。
	ListModels(ctx context.Context, p domain.Provider) ([]string, error)
}

// Forgetter 可选能力：删除 Provider 时清理适配器持有的锁/缓存/登录会话。
type Forgetter interface {
	Forget(providerID string)
}

// ProbeResult 模型测试的统一结果形状。
type ProbeResult struct {
	Level     string `json:"level"` // ok | warn | fail
	Message   string `json:"message"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Status    int    `json:"status,omitempty"`
}

// Registry 按 Provider.Kind 分发适配器。
type Registry struct {
	standard Adapter
	byKind   map[string]Adapter
}

// NewRegistry 创建注册表；standard 为标准 OpenAI 兼容适配器。
func NewRegistry(standard Adapter) *Registry {
	return &Registry{standard: standard, byKind: map[string]Adapter{}}
}

// Register 注册一个适配器（同一 Kind 后注册者覆盖先注册者）。
func (r *Registry) Register(a Adapter) {
	for _, k := range a.Kinds() {
		r.byKind[k] = a
	}
}

// For 返回该 Provider 对应的适配器；未注册的 Kind 回落到标准适配器。
func (r *Registry) For(p domain.Provider) Adapter {
	if a, ok := r.byKind[p.Kind]; ok {
		return a
	}
	return r.standard
}

// ForKind 按 Kind 字符串查找适配器（导入/登录等后台流程用）。
func (r *Registry) ForKind(kind string) (Adapter, bool) {
	a, ok := r.byKind[kind]
	return a, ok
}

// Adaptors 返回全部已注册的私有适配器。
func (r *Registry) Adaptors() []Adapter {
	seen := map[Adapter]bool{}
	out := make([]Adapter, 0, len(r.byKind))
	for _, a := range r.byKind {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Runtime 适配器共享的运行时依赖。
type Runtime struct {
	Store   *store.Store
	Clients *ClientFactory
	// Log 记录异常日志；由组合根注入，避免适配器依赖日志存储实现。
	Log func(domain.LogEntry)
	// Now 可注入的时钟（测试用）；nil = time.Now。
	Now func() time.Time
}

// Logf 记录一条日志（含 nil 保护，便于测试只构造部分依赖）。
func (r *Runtime) Logf(e domain.LogEntry) {
	if r != nil && r.Log != nil {
		r.Log(e)
	}
}

// NowTime 返回当前时间。
func (r *Runtime) NowTime() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
