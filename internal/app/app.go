// Package app 是网关的 HTTP 编排层：组合存储、会话、鉴权与上游适配器，
// 对外暴露代理入口（/v1/*）与后台接口（/api/*）。
package app

import (
	_ "embed"
	"log"
	"net/http"
	"time"

	"llmrouter/internal/upstream"
)

//go:embed web/index.html
var indexHTML []byte

// Config 启动配置。
type Config struct {
	Data       string
	AdminToken string
	APIToken   string
	TrustProxy bool
}

// New 加载状态并组装应用。
func New(cfg Config) (*Server, error) {
	st, err := LoadStore(cfg.Data)
	if err != nil {
		return nil, err
	}
	st.SetSaveErrorHandler(func(err error) { log.Printf("[ERROR] 状态文件落盘失败: %v", err) })
	st.SetAsyncWrites(true) // 用量/日志合并落盘；配置变更仍同步写
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}}
	return &Server{
		store:      st,
		client:     client,
		clients:    upstream.NewClientFactory(client),
		adminToken: cfg.AdminToken,
		apiToken:   cfg.APIToken,
		trustProxy: cfg.TrustProxy,
	}, nil
}

// Store 暴露状态存储（测试与诊断用）。
func (s *Server) Store() *Store { return s.store }

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler { return s.routes() }

// Version 当前版本号。
func Version() string { return appVersion }

// UpdateHelperArg 自动升级助手入口参数。
const UpdateHelperArg = updateHelperArg

// RunUpdateHelper 执行自动升级助手流程。
func RunUpdateHelper(args []string) { runUpdateHelper(args) }

// CleanupUpdateLeftovers 清理上次升级留下的文件。
func CleanupUpdateLeftovers() { cleanupUpdateLeftovers() }

// IsUpdateHelperArgs 判断当前进程是否是升级助手。
func IsUpdateHelperArgs(args []string) bool {
	return len(args) > 1 && args[1] == updateHelperArg
}

// StartBackground 启动后台任务（WorkBuddy 每日签到与余额快照）。
func (s *Server) StartBackground() { s.wbAdapter().CheckinLoop() }

// Shutdown 进程退出前落盘待写数据。
func (s *Server) Shutdown() { s.store.Flush() }
