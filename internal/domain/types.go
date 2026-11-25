// Package domain 定义网关的领域模型：上游、模型、路由组、密钥、用量与日志。
// 本包只包含纯数据类型和零依赖的值方法，不感知持久化、HTTP 或任何上游协议。
package domain

import (
	"encoding/json"
	"time"
)

// DefaultGroupID 默认路由组的固定 ID，不占用 Store 的 NextID（保持旧数据里 p1/m1 等 ID 序列不变）
const DefaultGroupID = "default"

// GroupMember 路由组成员：引用一条全局共享的模型配置，只有组内优先级是组特有的。
type GroupMember struct {
	ModelID  string `json:"model_id"`
	Priority int    `json:"priority"` // 组内优先级，越大越优先
}

// RouteGroup 一个路由组：对外暴露一个虚拟模型名，请求该名字时只在成员模型里按组内优先级选路。
// 模型配置本身全局共享，同一个模型可加入多个路由组，并在各组拥有不同优先级。
type RouteGroup struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`           // 对外模型名，客户端请求体 model 填这个名字
	Note    string        `json:"note,omitempty"` // 备注，仅后台展示
	Members []GroupMember `json:"members"`        // 成员模型及组内优先级
	// ContextLength 组外上下文窗口（tokens）的手动覆盖，0 = 自动取成员中已声明的最小值。
	// 只用于 /v1/models 对外声明（context_length / max_model_len），不参与路由。
	// 有些场景需要报小一些（给输出留余量）或报成员里最小值以外的值。
	ContextLength int64 `json:"context_length,omitempty"`
}

// Provider 一个上游服务商
type Provider struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	BaseURL      string           `json:"base_url"`                // chat completions 基址，填到 /v1，如 https://api.openai.com/v1
	ResponsesURL string           `json:"responses_url,omitempty"` // responses 协议基址，空 = 同 BaseURL
	APIKey       string           `json:"api_key,omitempty"`       // 上游密钥
	BalanceURL   string           `json:"balance_url,omitempty"`   // 可选的余额查询地址（GET + 本密钥）
	Proxy        string           `json:"proxy,omitempty"`         // 可选代理地址，支持 http(s):// 与 socks5(h)://
	Kind         string           `json:"kind,omitempty"`          // 上游类型：空 = 标准 OpenAI 兼容
	WB           *WorkBuddyCred   `json:"wb,omitempty"`            // WorkBuddy 凭证
	Zed          *ZedCred         `json:"zed,omitempty"`           // Zed 凭证
	CommandCode  *CommandCodeCred `json:"commandcode,omitempty"`   // Command Code 凭证
	Codex        *CodexCred       `json:"codex,omitempty"`         // Codex 套餐（Sign in with ChatGPT）凭证
	GeminiApp    *GeminiAppCred   `json:"geminiapp,omitempty"`     // Gemini App（Google 账号订阅）凭证
}

// WorkBuddyCred WorkBuddy 上游的账号凭证（本机客户端扫描导入或后台粘贴）。
// accessToken/refreshToken 与 uid 等字段名与官方客户端落盘的会话形状一致。
// 过期时刻用指针：time.Time 的 omitempty 不生效，零值会被序列化成 0001-01-01。
type WorkBuddyCred struct {
	UID              string     `json:"uid,omitempty"`
	Nickname         string     `json:"nickname,omitempty"`
	EnterpriseID     string     `json:"enterprise_id,omitempty"`
	Department       string     `json:"department,omitempty"`
	Domain           string     `json:"domain,omitempty"`
	TokenType        string     `json:"token_type,omitempty"`
	AccessToken      string     `json:"access_token,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`         // accessToken 过期时刻，nil = 未知
	RefreshExpiresAt *time.Time `json:"refresh_expires_at,omitempty"` // refreshToken 过期时刻，nil = 未知
	Usage            *WBUsage   `json:"usage,omitempty"`              // 最近一次余额快照（签到/查询时更新）
	LastCheckinDay   string     `json:"last_checkin_day,omitempty"`   // 最近一次签到成功的本地日期（2006-01-02）
}

// Expires 取 accessToken 过期时刻（nil 安全）
func (c *WorkBuddyCred) Expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// RefreshExpires 取 refreshToken 过期时刻（nil 安全）
func (c *WorkBuddyCred) RefreshExpires() time.Time {
	if c == nil || c.RefreshExpiresAt == nil {
		return time.Time{}
	}
	return *c.RefreshExpiresAt
}

// WBUsage WorkBuddy 余额快照（个人 = 各套餐周期剩余之和；企业 = 积分口径）
type WBUsage struct {
	Remain    float64   `json:"remain"`              // 剩余额度
	Total     *float64  `json:"total,omitempty"`     // 周期总额度；nil = 上游未暴露（不画进度）
	Used      *float64  `json:"used,omitempty"`      // 周期已用；nil = 同上
	Unlimited bool      `json:"unlimited,omitempty"` // 企业无限额度（limitNum=-1）
	CheckedAt time.Time `json:"checked_at"`
}

// ZedCred Zed 上游的账号凭证（后台原生登录获得）。NativeCredential 是 zed.dev
// 回调解密出的长期原生凭据（JSON 对象原文，作为整体参与鉴权）；AccessToken 是
// 用它换取的短期推理 JWT，临期由适配器用原生凭据重铸，无需刷新令牌。
type ZedCred struct {
	UserID           string          `json:"user_id"`
	SystemID         string          `json:"system_id,omitempty"`         // 本机 Zed 安装标识（可选）
	OrganizationID   string          `json:"organization_id,omitempty"`   // 铸 llm_tokens 的组织 UUID
	NativeCredential json.RawMessage `json:"native_credential,omitempty"` // 长期原生凭据 JSON 原文
	AccessToken      string          `json:"access_token,omitempty"`      // 短期推理 JWT
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"`        // JWT 过期时刻，nil = 未知
	Username         string          `json:"username,omitempty"`
	GitHubLogin      string          `json:"github_login,omitempty"`
	Plan             *ZedPlan        `json:"plan,omitempty"` // 最近一次账号/套餐快照
}

// ZedPlan Zed 账号套餐与模型请求用量快照
type ZedPlan struct {
	PlanType        string    `json:"plan_type,omitempty"`
	Used            *int64    `json:"used,omitempty"`  // 周期内已用模型请求数
	Limit           *int64    `json:"limit,omitempty"` // 周期上限，nil = 不限/未知
	SubscriptionEnd string    `json:"subscription_end,omitempty"`
	CheckedAt       time.Time `json:"checked_at"`
}

// Expires 取 JWT 过期时刻（nil 安全）
func (c *ZedCred) Expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// CommandCodeCred Command Code 订阅（commandcode.ai）的账号凭证。密钥长期有效，
// 无需刷新；Plan 是最近一次读取的套餐名，用于按套餐过滤模型。CLI 为 true 表示
// 密钥来自本机 `commandcode login` 的 ~/.commandcode/auth.json。
type CommandCodeCred struct {
	APIKey    string     `json:"api_key"`
	UserID    string     `json:"user_id,omitempty"`
	UserName  string     `json:"user_name,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	CLI       bool       `json:"cli,omitempty"`
	ZDR       bool       `json:"zdr,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// CodexCred Codex 套餐账号凭证，两种来源（Mode 区分）：
//   - ""（默认）= Sign in with ChatGPT 的 plan usage：OAuth 公共客户端动态注册，
//     access token（1 小时）临期用 refresh token（30 天滚动）续期，推理走
//     api.openai.com/v1/responses；
//   - "cli" = Codex CLI 的 ~/.codex/auth.json 登录态：client_id 固定为 Codex CLI
//     应用（app_…），刷新走 auth.openai.com/oauth/token，推理走
//     chatgpt.com/backend-api/codex/responses（带 chatgpt-account-id 等专用头）。
type CodexCred struct {
	Subject          string     `json:"subject,omitempty"` // ID token 的 sub，账号唯一标识
	Email            string     `json:"email,omitempty"`
	Name             string     `json:"name,omitempty"`
	Mode             string     `json:"mode,omitempty"`       // 空 = SIWC plan usage；"cli" = Codex CLI auth.json
	AccountID        string     `json:"account_id,omitempty"` // CLI 模式：chatgpt-account-id 头
	ClientID         string     `json:"client_id,omitempty"`  // 动态注册下发的正式 client_id；CLI 模式为固定 app_…
	AccessToken      string     `json:"access_token,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	IDToken          string     `json:"id_token,omitempty"` // 保留作 id_token_hint
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RefreshExpiresAt *time.Time `json:"refresh_expires_at,omitempty"`
	Scopes           []string   `json:"scopes,omitempty"`
	PlanUsage        bool       `json:"plan_usage,omitempty"` // 已授权推理（SIWC：chatgpt.tokens.use.direct；CLI 恒 true）
	CheckedAt        *time.Time `json:"checked_at,omitempty"` // 最近一次登录/刷新成功时刻
}

// Expires 取 access token 过期时刻（nil 安全）
func (c *CodexCred) Expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// RefreshExpires 取 refresh token 过期时刻（nil 安全）
func (c *CodexCred) RefreshExpires() time.Time {
	if c == nil || c.RefreshExpiresAt == nil {
		return time.Time{}
	}
	return *c.RefreshExpiresAt
}

// GeminiAppCred Gemini App（Google 账号订阅）凭证：浏览器 Google 登录（回环 PKCE）
// 或粘贴凭证 JSON 导入。refresh token 长期有效，access token（1 小时）临期用它续期。
// ClientID / ClientSecret 为空时用环境变量配置的 OAuth 客户端
// （LLMROUTER_GEMINI_CLIENT_ID / LLMROUTER_GEMINI_CLIENT_SECRET）；Usage 与 Terms
// 分别是最近一次用量快照与主服务条款的接受回执。
type GeminiAppCred struct {
	Subject      string          `json:"subject"` // userinfo 的 sub，账号唯一标识
	Email        string          `json:"email,omitempty"`
	ClientID     string          `json:"client_id,omitempty"` // 可选：覆盖环境变量里的 OAuth 客户端
	ClientSecret string          `json:"client_secret,omitempty"`
	AccessToken  string          `json:"access_token,omitempty"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	AuthError    string          `json:"auth_error,omitempty"` // 非空 = 授权已失效，需要重新登录
	Usage        *GeminiAppUsage `json:"usage,omitempty"`
	Terms        *GeminiAppTerms `json:"terms,omitempty"`
	CheckedAt    *time.Time      `json:"checked_at,omitempty"` // 最近一次登录/续期成功时刻
}

// Expires 取 access token 过期时刻（nil 安全）
func (c *GeminiAppCred) Expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// GeminiAppUsage Google GetUsageInfo 的只读快照：订阅档位与各用量窗口的已用百分比。
// 只反映 Google 上报的访问档位，不是账单或续订状态。
type GeminiAppUsage struct {
	Tier      string              `json:"tier,omitempty"` // free / pro / ultra / plus / neon
	Rows      []GeminiAppUsageRow `json:"rows,omitempty"`
	CheckedAt time.Time           `json:"checked_at"`
	Error     string              `json:"error,omitempty"` // 最近一次查询失败的原因（保留旧快照）
}

// GeminiAppUsageRow 一个用量窗口：Percent 是已用百分比，RefreshAt 是窗口滚动时刻
// （不保证归零）。
type GeminiAppUsageRow struct {
	Label     string     `json:"label"`
	Percent   float64    `json:"percent"`
	RefreshAt *time.Time `json:"refresh_at,omitempty"`
}

// GeminiAppTerms Gemini 主服务条款的接受回执：只记录本网关成功提交过接受，
// 不代表之后在官方 App 内的状态。
type GeminiAppTerms struct {
	Status     string    `json:"status"`            // accepted | error
	Version    int64     `json:"version,omitempty"` // 提交接受的条款版本号
	AcceptedAt time.Time `json:"accepted_at"`
	Error      string    `json:"error,omitempty"`
}

// MuteWindow 每天重复的静音时段，支持跨零点（如 22:00~06:00）
type MuteWindow struct {
	From string `json:"from"` // "HH:MM"
	To   string `json:"to"`
}

// Model 一条模型配置：某 provider 上的某个模型以及全部共享参数。
// 模型配置不隶属于任何路由组；路由组通过 RouteGroup.Members 引用它并只覆盖优先级。
type Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	// GroupID 仅用于读取旧版数据（每模型单组），加载迁移后恒为空。
	GroupID string `json:"group_id,omitempty"`
	Name    string `json:"name"` // 上游模型名：发往上游的 model 字段，也是直连名 provider/name 的后半段
	// Priority 直连优先级（已不参与直连选路，仅保留展示/兼容）：由各路由组成员
	// 优先级的最大值自动维护。
	Priority      int          `json:"priority"`
	DailyLimit    int64        `json:"daily_limit"`    // 每日 token 上限，0 = 不限（全局共享）
	MaxConcurrent int          `json:"max_concurrent"` // 最大并行请求数，0 = 不限；全局共享
	Mutes         []MuteWindow `json:"mutes,omitempty"`
	// ContextLength 上下文窗口（tokens），0 = 未声明。仅用于 /v1/models 向客户端
	// 声明（同时输出 context_length 与 max_model_len），不参与路由，也不改变透传行为。
	ContextLength int64 `json:"context_length,omitempty"`

	// Expose 该模型名是否出现在 /v1/models 列表里。
	Expose bool `json:"expose,omitempty"`

	// MutedUntil 上游返回 429 后自动静音到的时刻（绝对时间），零值 = 未静音。
	MutedUntil time.Time `json:"muted_until,omitempty"`
	// MuteReason 最近一次自动静音的原因说明（给后台展示）
	MuteReason string `json:"mute_reason,omitempty"`
}

// APIKey 客户端访问本网关 /v1/* 的密钥
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// UsageStat 按天 × key × provider × 模型 聚合的用量
type UsageStat struct {
	Date             string `json:"date"`
	KeyID            string `json:"key_id,omitempty"` // 空串 = 免鉴权，"env" = 环境变量令牌
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CachedTokens     int64  `json:"cached_tokens,omitempty"` // 输入中命中缓存的 token（含在 prompt_tokens 内）
	CompletionTokens int64  `json:"completion_tokens"`
}

// Balance 上游余额查询结果
type Balance struct {
	OK    bool      `json:"ok"`
	Raw   string    `json:"raw"`
	Found []string  `json:"found,omitempty"`
	At    time.Time `json:"at"`
}

// SessionStat 按天 × 会话 × key × provider × 模型 聚合的会话用量。
// 与 UsageStat 的区别：多一维会话 ID，并带上吞吐计时（生成耗时/首字节）。
type SessionStat struct {
	Date             string    `json:"date"`
	SessionID        string    `json:"session_id"`
	KeyID            string    `json:"key_id,omitempty"` // 空串 = 免鉴权，"env" = 环境变量令牌
	ProviderID       string    `json:"provider_id"`
	Model            string    `json:"model"`
	Requested        string    `json:"requested,omitempty"` // 客户端请求的模型名（auto 时与 Model 不同）
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CachedTokens     int64     `json:"cached_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens"`
	GenNanos         int64     `json:"gen_nanos,omitempty"`  // 累计生成耗时（流式 = 首字节→结束）
	TTFTNanos        int64     `json:"ttft_nanos,omitempty"` // 累计首字节耗时（仅流式）
	TTFTSamples      int64     `json:"ttft_samples,omitempty"`
	FirstAt          time.Time `json:"first_at"`
	LastAt           time.Time `json:"last_at"`

	// 思考等级：最近一次请求的客户端声明值 / 实际出站值（含网关默认注入与能力收敛）
	EffortDeclared string `json:"effort_declared,omitempty"`
	EffortSent     string `json:"effort_sent,omitempty"`
}

// Usage 一次调用的 token 用量
type Usage struct {
	Prompt     int64 // 输入总 tokens（含缓存命中部分）
	Cached     int64 // 其中命中缓存的 tokens
	Completion int64 // 输出 tokens
}

// Candidate 参与本次路由的一个候选（拷贝，避免和后台编辑竞态）
type Candidate struct {
	Provider Provider
	Model    Model
}

// SkipReason 某条模型路由被排除在候选之外的原因（用于会话日志说明选择理由）
type SkipReason struct {
	ModelID    string `json:"model_id"`
	Model      string `json:"model"`
	ProviderID string `json:"provider_id"`
	Reason     string `json:"reason"`
}

// Protocol 客户端接入协议：chat = /v1/chat/completions，responses = /v1/responses
type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
)
