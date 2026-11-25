package app

import (
	"net/http"

	"llmrouter/internal/upstream/geminiapp"
)

// geminiAdapter 构造 Gemini App 适配器。后台登录与代理请求可能并发触发首次
// 构造，故加锁（锁只护构造，适配器内部自己保证并发安全）。
func (s *Server) geminiAdapter() *geminiapp.Adapter {
	s.geminiMu.Lock()
	defer s.geminiMu.Unlock()
	if s.gemini == nil {
		s.gemini = geminiapp.NewWithBases(s.upstreamRuntime(), s.geminiOAuthBase, s.geminiOrigin, s.geminiUploadBase)
	}
	return s.gemini
}

func (s *Server) handleGeminiLoginStart(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiLoginStart(w, r)
}

func (s *Server) handleGeminiLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiLoginStatus(w, r)
}

func (s *Server) handleGeminiLoginPaste(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiLoginPaste(w, r)
}

func (s *Server) handleGeminiLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiLoginCancel(w, r)
}

func (s *Server) handleGeminiImport(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiImport(w, r)
}

func (s *Server) handleGeminiRefresh(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiRefresh(w, r)
}

// handleGeminiTerms 用户在后台显式确认后，代其向 Google 提交主服务条款的接受
func (s *Server) handleGeminiTerms(w http.ResponseWriter, r *http.Request) {
	s.geminiAdapter().HandleGeminiTerms(w, r)
}
