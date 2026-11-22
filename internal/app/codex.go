package app

import (
	"net/http"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/upstream/codex"
)

// codexOf 取 Codex 套餐凭证（Kind 不是 codex 时返回 nil）
func codexOf(p Provider) *CodexCred { return codex.Cred(p) }

// codexAdapter 构造 Codex 套餐适配器。
func (s *Server) codexAdapter() *codex.Adapter {
	if s.codex == nil {
		s.codex = codex.NewWithBases(s.upstreamRuntime(), s.codexAuthBase, s.codexAPIBase)
	}
	return s.codex
}

func (s *Server) handleCodexLoginStart(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexLoginStart(w, r)
}

func (s *Server) handleCodexLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexLoginStatus(w, r)
}

func (s *Server) handleCodexLoginPaste(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexLoginPaste(w, r)
}

func (s *Server) handleCodexLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexLoginCancel(w, r)
}

func (s *Server) handleCodexImport(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexImport(w, r)
}

func (s *Server) handleCodexRefresh(w http.ResponseWriter, r *http.Request) {
	s.codexAdapter().HandleCodexRefresh(w, r)
}

// codexAttempt 兼容旧调用点的入口（与 zedAttempt 同构）。
func (s *Server) codexAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *usageSample) error {
	proto := domain.ProtocolChat
	if responsesMode {
		proto = domain.ProtocolResponses
	}
	return s.codexAdapter().Attempt(&upstream.Attempt{
		Writer: w, Request: r, Candidate: c, Body: sendBody,
		KeyID: keyID, Protocol: proto, Timing: tm, Usage: sm,
	})
}
