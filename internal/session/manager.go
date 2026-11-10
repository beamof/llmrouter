// Package session 维护客户端会话与上游模型的粘性绑定。
// 绑定仅存内存，重启即清；按「会话 × 接入协议 × 请求模型名」隔离。
package session

import (
	"sort"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
)

// Binding 一条会话绑定及其吞吐统计。
type Binding struct {
	ModelID   string
	KeyID     string
	Requested string
	FirstAt   time.Time
	LastAt    time.Time
	Count     int
	Expires   time.Time

	OutTokens     int64
	GenNanos      int64
	LastTokPerSec float64
	TTFTNanos     int64
	TTFTSamples   int
	LastTTFT      time.Duration

	EffortDeclared string
	EffortSent     string
}

// Info 活跃会话的对外展示形状（GET /api/sessions）。
type Info struct {
	ID        string    `json:"id"`
	KeyID     string    `json:"key_id,omitempty"`
	ModelID   string    `json:"model_id"`
	Requested string    `json:"requested,omitempty"`
	Count     int       `json:"count"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	Expires   time.Time `json:"expires"`

	OutTokens     int64   `json:"out_tokens,omitempty"`
	TokPerSec     float64 `json:"tok_per_sec,omitempty"`
	LastTokPerSec float64 `json:"last_tok_per_sec,omitempty"`
	TTFTMs        int64   `json:"ttft_ms,omitempty"`
	LastTTFTMs    int64   `json:"last_ttft_ms,omitempty"`

	EffortDeclared string `json:"effort_declared,omitempty"`
	EffortSent     string `json:"effort_sent,omitempty"`
}

// Manager 会话绑定表。TTL 由外部注入（通常来自设置项）。
type Manager struct {
	mu  sync.Mutex
	m   map[string]Binding
	ttl func() time.Duration
}

// New 创建会话管理器；ttl 为空时使用默认 1 小时。
func New(ttl func() time.Duration) *Manager {
	if ttl == nil {
		ttl = func() time.Duration { return domain.DefaultSessionTTL }
	}
	return &Manager{m: map[string]Binding{}, ttl: ttl}
}

// Get 取绑定的模型 ID（已过期则删除）。
func (m *Manager) Get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(b.Expires) {
		delete(m.m, key)
		return "", false
	}
	return b.ModelID, true
}

// Set 建立或续期绑定，并累加成功请求数。
func (m *Manager) Set(key, modelID, keyID, requested string) {
	ttl := m.ttl()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]Binding{}
	}
	if len(m.m) > 1024 { // 顺手清理过期会话
		now := time.Now()
		for k, v := range m.m {
			if now.After(v.Expires) {
				delete(m.m, k)
			}
		}
	}
	now := time.Now()
	old := m.m[key]
	b := Binding{
		ModelID: modelID, KeyID: keyID, Requested: requested,
		FirstAt: old.FirstAt, Count: old.Count + 1,
		LastAt: now, Expires: now.Add(ttl),
		OutTokens: old.OutTokens, GenNanos: old.GenNanos, LastTokPerSec: old.LastTokPerSec,
		TTFTNanos: old.TTFTNanos, TTFTSamples: old.TTFTSamples, LastTTFT: old.LastTTFT,
		EffortDeclared: old.EffortDeclared, EffortSent: old.EffortSent,
	}
	if b.FirstAt.IsZero() {
		b.FirstAt = now
	}
	m.m[key] = b
}

// Clear 删除单个绑定。
func (m *Manager) Clear(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
}

// End 结束某个会话 ID 下的全部绑定，返回删除数量。
func (m *Manager) End(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		return 0
	}
	prefix := id + "\x00"
	n := 0
	for k := range m.m {
		if k == id || strings.HasPrefix(k, prefix) {
			delete(m.m, k)
			n++
		}
	}
	return n
}

// ApplyTTL 把新 TTL 立即应用到现有绑定。
func (m *Manager) ApplyTTL(ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, v := range m.m {
		exp := v.LastAt.Add(ttl)
		if now.After(exp) {
			delete(m.m, k)
			continue
		}
		v.Expires = exp
		m.m[k] = v
	}
}

// List 当前活跃绑定，按最近活跃倒序。
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]Info, 0, len(m.m))
	for k, v := range m.m {
		if now.After(v.Expires) {
			delete(m.m, k)
			continue
		}
		id := k // 粘性键 = 会话ID\x00协议\x00请求模型；展示用纯会话 ID
		if i := strings.IndexByte(k, 0x00); i >= 0 {
			id = k[:i]
		}
		info := Info{
			ID: id, KeyID: v.KeyID, ModelID: v.ModelID, Requested: v.Requested,
			Count: v.Count, FirstAt: v.FirstAt, LastAt: v.LastAt, Expires: v.Expires,
			OutTokens: v.OutTokens, LastTokPerSec: v.LastTokPerSec,
			LastTTFTMs:     int64(v.LastTTFT / time.Millisecond),
			EffortDeclared: v.EffortDeclared, EffortSent: v.EffortSent,
		}
		if v.GenNanos > 0 {
			info.TokPerSec = float64(v.OutTokens) / (float64(v.GenNanos) / float64(time.Second))
		}
		if v.TTFTSamples > 0 {
			info.TTFTMs = int64(time.Duration(v.TTFTNanos/int64(v.TTFTSamples)) / time.Millisecond)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}

// AddUsage 把一次成功请求的用量与计时并入会话统计。
func (m *Manager) AddUsage(key string, sm domain.CallSample) {
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok || time.Now().After(b.Expires) {
		return
	}
	if sm.Completion > 0 {
		b.OutTokens += sm.Completion
		b.GenNanos += int64(sm.Gen)
		b.LastTokPerSec = sm.TokPerSec()
	}
	if sm.Stream && sm.TTFT > 0 {
		b.TTFTNanos += int64(sm.TTFT)
		b.TTFTSamples++
		b.LastTTFT = sm.TTFT
	}
	b.EffortDeclared = sm.EffortDeclared
	b.EffortSent = sm.EffortSent
	m.m[key] = b
}

// Seed 直接写入一条绑定（测试与诊断用）。
func (m *Manager) Seed(key string, b Binding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]Binding{}
	}
	m.m[key] = b
}
