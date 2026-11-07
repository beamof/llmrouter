package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// 管理令牌防爆破：同一来源连续输错 adminMaxFails 次后锁定 adminLockout，
// 锁定期间不再校验令牌，直接返回令牌错误。
const (
	adminMaxFails = 5
	adminLockout  = 15 * time.Minute
)

// adminFail 某个来源的失败记录
type adminFail struct {
	count       int       // 连续失败次数
	lockedUntil time.Time // 锁定到期时间，零值 = 未锁定
	last        time.Time // 最近一次失败时间，用于清理
}

type Server struct {
	store      *Store
	client     *http.Client
	adminToken string // 后台令牌，空则不鉴权
	apiToken   string // 代理入口令牌，空则不鉴权
	trustProxy bool   // 是否信任 X-Forwarded-For / X-Real-IP 判定来源 IP

	sessMu   sync.Mutex
	sessions map[string]session // 会话粘性绑定，仅内存，重启即清

	wbMu    sync.Mutex
	wbLocks map[string]*sync.Mutex // WorkBuddy 续期单飞锁（键 = provider ID），仅内存
	wbCats  map[string]*wbCatalog  // WorkBuddy 模型目录缓存（键 = provider ID），仅内存

	failMu sync.Mutex
	fails  map[string]*adminFail // 管理令牌失败计数，按来源 IP，仅内存
}

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
	api.HandleFunc("POST /api/providers", s.handleProviderUpsert)
	api.HandleFunc("PUT /api/providers/{id}", s.handleProviderUpsert)
	api.HandleFunc("DELETE /api/providers/{id}", s.handleProviderDelete)
	api.HandleFunc("POST /api/providers/{id}/balance", s.handleBalance)
	api.HandleFunc("POST /api/providers/{id}/wb-refresh", s.handleWBRefresh)
	api.HandleFunc("GET /api/providers/{id}/models", s.handleUpstreamModels)
	api.HandleFunc("POST /api/workbuddy/scan", s.handleWBScan)
	api.HandleFunc("POST /api/workbuddy/import", s.handleWBImport)
	api.HandleFunc("POST /api/models", s.handleModelUpsert)
	api.HandleFunc("PUT /api/models/{id}", s.handleModelUpsert)
	api.HandleFunc("DELETE /api/models/{id}", s.handleModelDelete)
	api.HandleFunc("POST /api/models/reorder", s.handleReorder)
	api.HandleFunc("POST /api/models/{id}/test", s.handleModelTest)
	api.HandleFunc("DELETE /api/models/{id}/mute", s.handleModelUnmute)
	api.HandleFunc("POST /api/keys", s.handleKeyCreate)
	api.HandleFunc("DELETE /api/keys/{id}", s.handleKeyDelete)
	api.HandleFunc("PUT /api/settings", s.handleSettings)
	api.HandleFunc("DELETE /api/logs", s.handleLogsClear)
	mux.Handle("/api/", s.adminAuth(api))
	return mux
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		ip := s.clientIP(r)
		if until, locked := s.adminLocked(ip); locked {
			secs := int(time.Until(until).Seconds()) + 1
			s.logFail(LogEntry{
				Level: "warn", Kind: kindAdminLocked, Status: http.StatusUnauthorized,
				Message: "管理令牌锁定期间的访问尝试",
				Detail:  fmt.Sprintf("来源 %s，剩余 %d 秒", ip, secs),
			})
			writeErr(w, http.StatusUnauthorized, fmt.Sprintf("尝试次数过多，请 %d 秒后再试", secs))
			return
		}
		tok := r.Header.Get("X-Admin-Token")
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) != 1 {
			locked := s.recordAdminFail(ip)
			msg := "需要正确的管理令牌"
			if locked {
				msg = fmt.Sprintf("令牌错误次数过多，已锁定 %d 分钟", int(adminLockout.Minutes()))
			} else if n := s.adminFailCount(ip); n > 0 && adminMaxFails-n <= 2 {
				msg = fmt.Sprintf("管理令牌错误，再错 %d 次将锁定", adminMaxFails-n)
			}
			s.logFail(LogEntry{
				Level: "warn", Kind: kindAdminAuth, Status: http.StatusUnauthorized,
				Message: msg,
				Detail:  fmt.Sprintf("来源 %s，%s %s", ip, r.Method, r.URL.Path),
			})
			writeErr(w, http.StatusUnauthorized, msg)
			return
		}
		s.clearAdminFail(ip)
		next.ServeHTTP(w, r)
	})
}

// clientIP 判定请求来源，用作失败计数与锁定的键。
// 默认取 TCP 对端地址；只有显式开启 -trust-proxy（部署在反向代理之后）才采信转发头，
// 否则客户端可伪造 X-Forwarded-For 绕过计数。
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// adminLocked 该来源是否仍在锁定期内（调用方无需持锁）
func (s *Server) adminLocked(ip string) (time.Time, bool) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	f, ok := s.fails[ip]
	if !ok || f.lockedUntil.IsZero() {
		return time.Time{}, false
	}
	if time.Now().Before(f.lockedUntil) {
		return f.lockedUntil, true
	}
	// 锁定已过期，重置该来源的记录
	delete(s.fails, ip)
	return time.Time{}, false
}

// recordAdminFail 记一次失败，返回本次是否触发锁定
func (s *Server) recordAdminFail(ip string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.fails == nil {
		s.fails = map[string]*adminFail{}
	}
	now := time.Now()
	f, ok := s.fails[ip]
	if !ok {
		f = &adminFail{}
		s.fails[ip] = f
	}
	f.count++
	f.last = now
	if f.count >= adminMaxFails {
		f.lockedUntil = now.Add(adminLockout)
	}
	if len(s.fails) > 1024 { // 顺手清理长期无活动的记录，避免内存无界增长
		for k, v := range s.fails {
			if v.lockedUntil.IsZero() && now.Sub(v.last) > adminLockout {
				delete(s.fails, k)
			}
		}
	}
	return !f.lockedUntil.IsZero()
}

// adminFailCount 当前来源的连续失败次数
func (s *Server) adminFailCount(ip string) int {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if f, ok := s.fails[ip]; ok {
		return f.count
	}
	return 0
}

// clearAdminFail 令牌校验通过后清空该来源的失败记录
func (s *Server) clearAdminFail(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	delete(s.fails, ip)
}

// checkAPIToken 校验客户端密钥，返回命中的 key ID（"env" = 环境变量令牌，空串 = 免鉴权放行）
func (s *Server) checkAPIToken(r *http.Request) (string, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok != "" && s.apiToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.apiToken)) == 1 {
		return "env", true
	}
	if id, ok := s.store.MatchAPIKey(tok); ok {
		return id, true
	}
	// 未启用鉴权（没有 Key、没有环境变量令牌）时保持开放，客户端自带的 Authorization 原样透传（BYOK）
	if s.apiToken == "" && !s.store.HasAPIKeys() {
		return "", true
	}
	return "", false
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "llmrouter_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", getenv("LLMROUTER_ADDR", "127.0.0.1:8080"), "监听地址")
	data := flag.String("data", getenv("LLMROUTER_DATA", "data.json"), "状态文件路径")
	adminToken := flag.String("admin-token", os.Getenv("LLMROUTER_ADMIN_TOKEN"), "管理后台令牌（空 = 不鉴权）")
	apiToken := flag.String("api-token", os.Getenv("LLMROUTER_API_TOKEN"), "代理入口令牌（空 = 不鉴权）")
	trustProxy := flag.Bool("trust-proxy", os.Getenv("LLMROUTER_TRUST_PROXY") != "", "部署在反向代理后时开启：按 X-Forwarded-For 判定来源 IP（默认取 TCP 对端）")
	flag.Parse()

	store, err := LoadStore(*data)
	if err != nil {
		log.Fatalf("读取 %s 失败: %v", *data, err)
	}
	s := &Server{
		store:      store,
		client:     &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}},
		adminToken: *adminToken,
		apiToken:   *apiToken,
		trustProxy: *trustProxy,
	}
	if s.adminToken == "" {
		log.Println("提示：未设置管理令牌，后台完全开放；建议 -admin-token XXX 或环境变量 LLMROUTER_ADMIN_TOKEN")
	}
	if !store.HasAPIKeys() && s.apiToken == "" {
		log.Println("提示：未配置任何 API Key，代理入口不鉴权；可在后台 Keys 页生成")
	}
	log.Printf("LLM Router 已启动：管理后台 http://%s | 代理入口 /v1/chat/completions /v1/responses /v1/models", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.routes()))
}
