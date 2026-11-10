package domain

import "time"

// RateLimitMute 上游返回 429 后自动静音的默认时长
const RateLimitMute = 5 * time.Hour

// DefaultSessionTTL 会话粘性的默认滑动过期时间
const DefaultSessionTTL = time.Hour

// MaxSessionTTL 会话粘性有效期上限，避免超大值在 time.Duration 换算时溢出
const MaxSessionTTL = 30 * 24 * time.Hour

// LogEntry 一次异常、失败或会话建立记录。
type LogEntry struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`  // info | warn | error
	Kind       string    `json:"kind"`   // 记录类型，见下面 Kind* 常量
	Status     int       `json:"status"` // 相关 HTTP 状态码
	KeyID      string    `json:"key_id,omitempty"`
	ProviderID string    `json:"provider_id,omitempty"`
	Model      string    `json:"model,omitempty"`
	Message    string    `json:"message"`          // 简要说明
	Detail     string    `json:"detail,omitempty"` // 上游错误详情（已截断）

	// 会话建立相关（kind = session）
	SessionID  string       `json:"session_id,omitempty"`
	Protocol   string       `json:"protocol,omitempty"`
	Requested  string       `json:"requested,omitempty"`
	Switched   bool         `json:"switched,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Candidates []string     `json:"candidates,omitempty"`
	Skipped    []SkipReason `json:"skipped,omitempty"`
}

// 记录类型
const (
	KindAuth        = "auth"         // 客户端 API Key 无效
	KindBadRequest  = "bad_request"  // 请求体不合法
	KindNoCandidate = "no_candidate" // 没有可用上游
	KindUpstream    = "upstream"     // 单个上游调用失败（可能已故障转移到下一个）
	KindAllFailed   = "all_failed"   // 所有上游都失败，返回 502
	KindAdminAuth   = "admin_auth"   // 后台令牌错误
	KindAdminLocked = "admin_locked" // 后台令牌因连续错误被锁定
	KindSession     = "session"      // 会话建立/切换绑定
	KindMuted       = "muted"        // 上游 429，模型被自动静音
	KindCheckin     = "checkin"      // WorkBuddy 签到/余额刷新结果
	KindUpdate      = "update"       // 版本检测/自动升级结果
)

// TruncateRunes 按字符截断，避免切断多字节字符
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
