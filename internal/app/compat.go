package app

// 迁移期兼容层：根包暂时保留旧短名称，随着各模块迁入 internal/ 逐步删除。
// 新代码一律使用 domain / store 的显式类型名。

import (
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/store"
)

type (
	Store           = store.Store
	GroupMember     = domain.GroupMember
	RouteGroup      = domain.RouteGroup
	Provider        = domain.Provider
	WorkBuddyCred   = domain.WorkBuddyCred
	ZedCred         = domain.ZedCred
	ZedPlan         = domain.ZedPlan
	CommandCodeCred = domain.CommandCodeCred
	MuteWindow      = domain.MuteWindow
	Model           = domain.Model
	APIKey          = domain.APIKey
	UsageStat       = domain.UsageStat
	Balance         = domain.Balance
	SessionStat     = domain.SessionStat
	Usage           = domain.Usage
	Candidate       = domain.Candidate
	SkipReason      = domain.SkipReason
	LogEntry        = domain.LogEntry
	WBUsage         = domain.WBUsage
	usageSample     = domain.CallSample
)

const (
	rateLimitMute   = domain.RateLimitMute
	kindAuth        = domain.KindAuth
	kindBadRequest  = domain.KindBadRequest
	kindNoCandidate = domain.KindNoCandidate
	kindUpstream    = domain.KindUpstream
	kindAllFailed   = domain.KindAllFailed
	kindAdminAuth   = domain.KindAdminAuth
	kindAdminLocked = domain.KindAdminLocked
	kindSession     = domain.KindSession
	kindMuted       = domain.KindMuted
	kindCheckin     = domain.KindCheckin
	kindUpdate      = domain.KindUpdate
	defaultGroupID  = domain.DefaultGroupID
	maxSessionTTL   = domain.MaxSessionTTL
)

func truncateRunes(s string, max int) string { return domain.TruncateRunes(s, max) }

func mutedNow(ws []MuteWindow, now time.Time) bool { return domain.MutedNow(ws, now) }

func mutedUntil(m *Model, now time.Time) bool { return domain.MutedUntil(m, now) }

func LoadStore(path string) (*Store, error) { return store.LoadStore(path) }

var _ = time.Second
