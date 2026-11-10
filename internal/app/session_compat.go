package app

import (
	"time"

	"llmrouter/internal/session"
	"llmrouter/internal/wire"
)

// sessionInfo 兼容旧调用点。
type sessionInfo = session.Info

// sessionsMgr 返回会话管理器（TTL 跟随设置项）。
func (s *Server) sessionsMgr() *session.Manager {
	if s.sessions == nil {
		s.sessions = session.New(func() time.Duration {
			if s.store == nil {
				return time.Hour
			}
			return s.store.SessionTTL()
		})
	}
	return s.sessions
}

func (s *Server) getSession(key string) (string, bool) { return s.sessionsMgr().Get(key) }

func (s *Server) setSession(key, modelID, keyID, requested string) {
	s.sessionsMgr().Set(key, modelID, keyID, requested)
}

func (s *Server) clearSession(key string) { s.sessionsMgr().Clear(key) }

func (s *Server) endSession(id string) int { return s.sessionsMgr().End(id) }

func (s *Server) applySessionTTL(ttl time.Duration) { s.sessionsMgr().ApplyTTL(ttl) }

func (s *Server) activeSessions() []session.Info { return s.sessionsMgr().List() }

func (s *Server) addSessionUsage(key string, sm usageSample) { s.sessionsMgr().AddUsage(key, sm) }

func effortArg(body map[string]any) (string, bool) { return wire.EffortArg(body) }

func requestEffort(body []byte) string { return wire.RequestEffort(body) }
