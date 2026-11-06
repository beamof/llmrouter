package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type Server struct {
	store      *Store
	client     *http.Client
	adminToken string // 后台令牌，空则不鉴权
	apiToken   string // 代理入口令牌，空则不鉴权

	sessMu   sync.Mutex
	sessions map[string]session // 会话粘性绑定，仅内存，重启即清
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
	api.HandleFunc("GET /api/providers/{id}/models", s.handleUpstreamModels)
	api.HandleFunc("POST /api/models", s.handleModelUpsert)
	api.HandleFunc("PUT /api/models/{id}", s.handleModelUpsert)
	api.HandleFunc("DELETE /api/models/{id}", s.handleModelDelete)
	api.HandleFunc("POST /api/models/reorder", s.handleReorder)
	api.HandleFunc("POST /api/models/{id}/test", s.handleModelTest)
	api.HandleFunc("POST /api/keys", s.handleKeyCreate)
	api.HandleFunc("DELETE /api/keys/{id}", s.handleKeyDelete)
	api.HandleFunc("PUT /api/settings", s.handleSettings)
	mux.Handle("/api/", s.adminAuth(api))
	return mux
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken != "" {
			tok := r.Header.Get("X-Admin-Token")
			if tok == "" {
				tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) != 1 {
				writeErr(w, http.StatusUnauthorized, "需要正确的管理令牌")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
