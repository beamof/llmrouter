// Package auth 负责客户端 API Key 校验与后台管理令牌鉴权（含防爆破锁定）。
package auth

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/httpx"
)

const (
	// MaxFails 连续错误次数上限
	MaxFails = 5
	// Lockout 锁定时长
	Lockout = 15 * time.Minute

	adminMaxFails = MaxFails
	adminLockout  = Lockout
)

type fail struct {
	count       int
	lockedUntil time.Time
	last        time.Time
}

// Guard 管理令牌鉴权与来源失败计数。
type Guard struct {
	AdminToken string
	TrustProxy bool
	Log        func(domain.LogEntry)

	mu    sync.Mutex
	fails map[string]*fail
}

// NewGuard 创建鉴权守卫。
func NewGuard(adminToken string, trustProxy bool, log func(domain.LogEntry)) *Guard {
	return &Guard{AdminToken: adminToken, TrustProxy: trustProxy, Log: log, fails: map[string]*fail{}}
}

// Middleware 管理令牌中间件；令牌为空时放行。
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		ip := g.ClientIP(r)
		if until, locked := g.locked(ip); locked {
			secs := int(time.Until(until).Seconds()) + 1
			g.log(domain.LogEntry{
				Level: "warn", Kind: domain.KindAdminLocked, Status: http.StatusUnauthorized,
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
		if subtle.ConstantTimeCompare([]byte(tok), []byte(g.AdminToken)) != 1 {
			locked := g.recordFail(ip)
			msg := "需要正确的管理令牌"
			if locked {
				msg = fmt.Sprintf("令牌错误次数过多，已锁定 %d 分钟", int(adminLockout.Minutes()))
			} else if n := g.FailCount(ip); n > 0 && adminMaxFails-n <= 2 {
				msg = fmt.Sprintf("管理令牌错误，再错 %d 次将锁定", adminMaxFails-n)
			}
			g.log(domain.LogEntry{
				Level: "warn", Kind: domain.KindAdminAuth, Status: http.StatusUnauthorized,
				Message: msg,
				Detail:  fmt.Sprintf("来源 %s，%s %s", ip, r.Method, r.URL.Path),
			})
			writeErr(w, http.StatusUnauthorized, msg)
			return
		}
		g.clear(ip)
		next.ServeHTTP(w, r)
	})
}

// ClientIP 判定请求来源，用作失败计数与锁定的键。
// 默认取 TCP 对端地址；只有显式开启 trust-proxy 才采信转发头。
func (g *Guard) ClientIP(r *http.Request) string {
	if g.TrustProxy {
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

func (g *Guard) locked(ip string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.fails[ip]
	if !ok || f.lockedUntil.IsZero() {
		return time.Time{}, false
	}
	if time.Now().Before(f.lockedUntil) {
		return f.lockedUntil, true
	}
	delete(g.fails, ip)
	return time.Time{}, false
}

func (g *Guard) recordFail(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fails == nil {
		g.fails = map[string]*fail{}
	}
	now := time.Now()
	f, ok := g.fails[ip]
	if !ok {
		f = &fail{}
		g.fails[ip] = f
	}
	f.count++
	f.last = now
	if f.count >= adminMaxFails {
		f.lockedUntil = now.Add(adminLockout)
	}
	if len(g.fails) > 1024 {
		for k, v := range g.fails {
			if v.lockedUntil.IsZero() && now.Sub(v.last) > adminLockout {
				delete(g.fails, k)
			}
		}
	}
	return !f.lockedUntil.IsZero()
}

// FailCount 当前来源的连续失败次数。
func (g *Guard) FailCount(ip string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.fails[ip]; ok {
		return f.count
	}
	return 0
}

func (g *Guard) clear(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

func (g *Guard) log(e domain.LogEntry) {
	if g.Log != nil {
		g.Log(e)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) { httpx.WriteErr(w, status, msg) }

// KeyStore 校验客户端 API Key 所需的最小存储接口。
type KeyStore interface {
	MatchAPIKey(token string) (string, bool)
	HasAPIKeys() bool
}

// CheckAPIToken 校验客户端密钥，返回命中的 key ID（"env" = 环境变量令牌，空串 = 免鉴权放行）。
func CheckAPIToken(r *http.Request, apiToken string, st KeyStore) (string, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok != "" && apiToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(apiToken)) == 1 {
		return "env", true
	}
	if id, ok := st.MatchAPIKey(tok); ok {
		return id, true
	}
	// 未启用鉴权（没有 Key、没有环境变量令牌）时保持开放，客户端自带的 Authorization 原样透传（BYOK）
	if apiToken == "" && !st.HasAPIKeys() {
		return "", true
	}
	return "", false
}

// FailState 单个来源的失败状态快照。
type FailState struct {
	Count       int
	LockedUntil time.Time
	Last        time.Time
}

// Failures 返回全部来源的失败状态快照。
func (g *Guard) Failures() map[string]FailState {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]FailState, len(g.fails))
	for k, v := range g.fails {
		out[k] = FailState{Count: v.count, LockedUntil: v.lockedUntil, Last: v.last}
	}
	return out
}

// SetLockedUntil 调整某个来源的锁定到期时间（测试用）。
func (g *Guard) SetLockedUntil(ip string, t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.fails[ip]; ok {
		f.lockedUntil = t
	}
}
