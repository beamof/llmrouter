package app

import (
	"net/http"
	"sync"
	"time"

	"llmrouter/internal/auth"
	"llmrouter/internal/session"
	"llmrouter/internal/upstream"
	"llmrouter/internal/upstream/codex"
	"llmrouter/internal/upstream/workbuddy"
	"llmrouter/internal/upstream/zed"
)

// Server 网关运行时：存储、会话、鉴权、上游适配器与后台任务状态。
type Server struct {
	store   *Store
	client  *http.Client
	clients *upstream.ClientFactory // 按 Provider 代理配置构造的上游客户端
	reg     *upstream.Registry      // 上游适配器注册表

	adminToken string // 后台令牌，空则不鉴权
	apiToken   string // 代理入口令牌，空则不鉴权
	trustProxy bool   // 是否信任 X-Forwarded-For / X-Real-IP 判定来源 IP

	sessions *session.Manager // 会话粘性绑定（仅内存，重启即清）

	wb            *workbuddy.Adapter // WorkBuddy 适配器（含续期单飞锁与目录缓存）
	zed           *zed.Adapter       // Zed 适配器（含重铸单飞锁与登录会话）
	zedBase       string             // Zed 云端基址覆盖（测试注入用，空 = 官方端点）
	ccBase        string             // Command Code 基址覆盖（测试注入用，空 = 官方端点）
	codex         *codex.Adapter     // Codex 套餐适配器（含续期单飞锁与登录会话）
	codexAuthBase string             // Codex 授权服务器基址覆盖（测试注入用，空 = 官方端点）
	codexAPIBase  string             // Codex 推理 API 基址覆盖（测试注入用，空 = 官方端点）

	guard *auth.Guard // 后台鉴权与失败锁定

	updMu      sync.Mutex
	upd        *updateInfo // 最近一次版本检测结果（缓存），nil = 未检测
	updAt      time.Time   // 缓存写入时间
	updRunning bool        // 升级任务是否正在执行
	updTask    *updateTask // 当前/最近一次升级任务的实时状态（前端轮询）
	// installUpdate 完成安装收尾（平台准备 + 交接重启）；测试注入用，nil = 默认实现。
	installUpdate func(newPath, exe string) error
	ghAPI         string // GitHub API 基址覆盖（测试注入，空 = 官方端点）
}
