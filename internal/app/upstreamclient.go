package app

import (
	"net/http"
	"time"

	"llmrouter/internal/upstream"
)

// normalizeProxy 兼容旧调用点（实现见 internal/upstream）。
func normalizeProxy(raw string) (string, error) { return upstream.NormalizeProxy(raw) }

// upstreamClient 兼容旧调用点：按 Provider 代理配置构造客户端。
func (s *Server) upstreamClient(p Provider, timeout time.Duration) (*http.Client, error) {
	if s.clients == nil {
		s.clients = upstream.NewClientFactory(s.client)
	}
	return s.clients.For(p, timeout)
}
