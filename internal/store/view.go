package store

// ModelView 模型 + 今日已用 tokens + 当前在途请求数（给后台页面）。
// ExternalName 是直连请求应使用的对外名 "provider/model"，与 Model.Name（上游模型名）区分。
type ModelView struct {
	*Model
	ExternalName string `json:"external_name"`
	UsedToday    int64  `json:"used_today"`
	Inflight     int    `json:"inflight"`
}

// StateView 后台页面所需的完整状态
type StateView struct {
	Providers []*Provider   `json:"providers"`
	Groups    []*RouteGroup `json:"groups"`
	Models    []ModelView   `json:"models"`
	APIKeys   []*APIKey     `json:"api_keys"`
	Usage     []*UsageStat  `json:"usage"`
	// SessionUsage 按天 × 会话 聚合的用量，含已过期会话
	SessionUsage []*SessionStat      `json:"session_usage"`
	Balances     map[string]*Balance `json:"balances"`
	Logs         []*LogEntry         `json:"logs"`
	AutoModel    string              `json:"auto_model"`
	// ResponsesConvert responses 协议转换开关（默认关闭）
	ResponsesConvert bool `json:"responses_convert"`
	// SessionTTLSeconds 生效中的会话粘性有效期（秒）
	SessionTTLSeconds int64 `json:"session_ttl_seconds"`
}
