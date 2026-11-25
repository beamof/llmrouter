package app

import (
	"net/http"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/upstream/workbuddy"
)

// upstreamRuntime 组装适配器共享的运行时依赖。
// 迁移期保留在这里；组合根落地后由 internal/app 统一注入。
func (s *Server) upstreamRuntime() *upstream.Runtime {
	if s.clients == nil {
		s.clients = upstream.NewClientFactory(s.client)
	}
	return &upstream.Runtime{Store: s.store, Clients: s.clients, Log: s.logFail}
}

// wbAdapter 构造 WorkBuddy 适配器（锁与目录缓存归属于该实例）。
func (s *Server) wbAdapter() *workbuddy.Adapter {
	if s.wb == nil {
		s.wb = workbuddy.New(s.upstreamRuntime())
	}
	return s.wb
}

// wbAttempt 兼容旧候选循环的入口。
func (s *Server) wbAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *usageSample) error {
	proto := domain.ProtocolChat
	if responsesMode {
		proto = domain.ProtocolResponses
	}
	return s.wbAdapter().Attempt(&upstream.Attempt{
		Writer: w, Request: r, Candidate: c, Body: sendBody,
		KeyID: keyID, Protocol: proto, Timing: tm, Usage: sm,
	})
}

// registry 组装上游适配器注册表（标准上游 + 各私有协议）。
// 登录/代理/后台请求都可能并发触发首次构造，用 sync.Once 保证只构造一份。
func (s *Server) registry() *upstream.Registry {
	s.regOnce.Do(func() {
		std := &upstream.Standard{Store: s.store, Clients: s.upstreamRuntime().Clients, APIToken: s.apiToken}
		r := upstream.NewRegistry(std)
		r.Register(s.wbAdapter())
		r.Register(s.zedAdapter())
		r.Register(s.ccAdapter())
		r.Register(s.codexAdapter())
		r.Register(s.geminiAdapter())
		s.reg = r
	})
	return s.reg
}
