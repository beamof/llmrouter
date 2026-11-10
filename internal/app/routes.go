package app

import (
	"net/http"

	"llmrouter/internal/auth"
)

const (
	adminMaxFails = auth.MaxFails
	adminLockout  = auth.Lockout
)

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("POST /v1/chat/completions", s.handleProxy("/chat/completions"))
	mux.HandleFunc("POST /v1/responses", s.handleProxy("/responses"))
	mux.HandleFunc("GET /v1/models", s.handleModels)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.replyState)
	api.HandleFunc("GET /api/sessions", s.handleSessions)
	api.HandleFunc("DELETE /api/sessions", s.handleSessionEnd)
	api.HandleFunc("POST /api/providers", s.handleProviderUpsert)
	api.HandleFunc("PUT /api/providers/{id}", s.handleProviderUpsert)
	api.HandleFunc("DELETE /api/providers/{id}", s.handleProviderDelete)
	api.HandleFunc("POST /api/providers/{id}/balance", s.handleBalance)
	api.HandleFunc("POST /api/providers/{id}/wb-refresh", s.handleWBRefresh)
	api.HandleFunc("POST /api/providers/{id}/wb-checkin", s.handleWBCheckin)
	api.HandleFunc("POST /api/providers/{id}/wb-usage", s.handleWBUsage)
	api.HandleFunc("GET /api/providers/{id}/models", s.handleUpstreamModels)
	api.HandleFunc("POST /api/workbuddy/scan", s.handleWBScan)
	api.HandleFunc("POST /api/workbuddy/import", s.handleWBImport)
	api.HandleFunc("POST /api/zed/login", s.handleZedLoginStart)
	api.HandleFunc("GET /api/zed/login/status", s.handleZedLoginStatus)
	api.HandleFunc("POST /api/zed/login/paste", s.handleZedLoginPaste)
	api.HandleFunc("POST /api/zed/login/cancel", s.handleZedLoginCancel)
	api.HandleFunc("POST /api/providers/{id}/zed-refresh", s.handleZedRefresh)
	api.HandleFunc("POST /api/commandcode/import", s.handleCommandCodeImport)
	api.HandleFunc("POST /api/providers/{id}/commandcode-refresh", s.handleCommandCodeRefresh)
	api.HandleFunc("POST /api/groups", s.handleGroupCreate)
	api.HandleFunc("PUT /api/groups/{id}", s.handleGroupUpdate)
	api.HandleFunc("DELETE /api/groups/{id}", s.handleGroupDelete)
	api.HandleFunc("POST /api/groups/{id}/models", s.handleGroupAttachModel)
	api.HandleFunc("DELETE /api/groups/{id}/models/{modelID}", s.handleGroupDetachModel)
	api.HandleFunc("POST /api/models", s.handleModelUpsert)
	api.HandleFunc("PUT /api/models/{id}", s.handleModelUpsert)
	api.HandleFunc("DELETE /api/models/{id}", s.handleModelDelete)
	api.HandleFunc("POST /api/models/{id}/expose", s.handleModelExpose)
	api.HandleFunc("POST /api/models/reorder", s.handleReorder)
	api.HandleFunc("POST /api/models/{id}/test", s.handleModelTest)
	api.HandleFunc("DELETE /api/models/{id}/mute", s.handleModelUnmute)
	api.HandleFunc("POST /api/keys", s.handleKeyCreate)
	api.HandleFunc("DELETE /api/keys/{id}", s.handleKeyDelete)
	api.HandleFunc("PUT /api/settings", s.handleSettings)
	api.HandleFunc("DELETE /api/logs", s.handleLogsClear)
	api.HandleFunc("GET /api/version", s.handleVersion)
	api.HandleFunc("GET /api/update/check", s.handleUpdateCheck)
	api.HandleFunc("POST /api/update/apply", s.handleUpdateApply)
	mux.Handle("/api/", s.adminAuth(api))
	return mux
}

// adminAuth 后台管理令牌鉴权（实现见 internal/auth）。
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.authGuard().Middleware(next).ServeHTTP(w, r)
	})
}

// clientIP 客户端来源 IP。
func (s *Server) clientIP(r *http.Request) string { return s.authGuard().ClientIP(r) }

// checkAPIToken 校验客户端密钥（环境变量令牌或已生成的 Key）。
func (s *Server) checkAPIToken(r *http.Request) (string, bool) {
	return auth.CheckAPIToken(r, s.apiToken, s.store)
}

// authGuard 返回鉴权守卫（懒初始化并同步当前配置）。
func (s *Server) authGuard() *auth.Guard {
	if s.guard == nil {
		s.guard = auth.NewGuard(s.adminToken, s.trustProxy, s.logFail)
	}
	s.guard.AdminToken, s.guard.TrustProxy = s.adminToken, s.trustProxy
	return s.guard
}
