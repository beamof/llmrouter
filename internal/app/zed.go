package app

import (
	"net/http"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/upstream/zed"
)

// zedOf 取 Zed 凭证（Kind 不是 zed 时返回 nil）
func zedOf(p Provider) *ZedCred { return zed.Cred(p) }

// zedAdapter 构造 Zed 适配器。
func (s *Server) zedAdapter() *zed.Adapter {
	if s.zed == nil {
		s.zed = zed.New(s.upstreamRuntime(), s.zedBase)
	}
	return s.zed
}

// zedCatalog 兼容旧调用点。
func (s *Server) zedCatalog(p Provider) ([]string, error) { return s.zedAdapter().Catalog(p) }

// zedForgetProvider 兼容旧调用点。
func (s *Server) zedForgetProvider(id string) { s.zedAdapter().Forget(id) }

// zedAttempt 兼容旧候选循环的入口。
func (s *Server) zedAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *usageSample) error {
	proto := domain.ProtocolChat
	if responsesMode {
		proto = domain.ProtocolResponses
	}
	return s.zedAdapter().Attempt(&upstream.Attempt{
		Writer: w, Request: r, Candidate: c, Body: sendBody,
		KeyID: keyID, Protocol: proto, Timing: tm, Usage: sm,
	})
}

func (s *Server) handleZedLoginStart(w http.ResponseWriter, r *http.Request) {
	s.zedAdapter().HandleZedLoginStart(w, r)
}

func (s *Server) handleZedLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.zedAdapter().HandleZedLoginStatus(w, r)
}

func (s *Server) handleZedLoginPaste(w http.ResponseWriter, r *http.Request) {
	s.zedAdapter().HandleZedLoginPaste(w, r)
}

func (s *Server) handleZedLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.zedAdapter().HandleZedLoginCancel(w, r)
}

func (s *Server) handleZedRefresh(w http.ResponseWriter, r *http.Request) {
	s.zedAdapter().HandleZedRefresh(w, r)
}
