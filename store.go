package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Provider 一个上游服务商
type Provider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`                // chat completions 基址，填到 /v1，如 https://api.openai.com/v1
	ResponsesURL string `json:"responses_url,omitempty"` // responses 协议基址，空 = 同 BaseURL（如智谱两协议不同基址时分别填写）
	APIKey       string `json:"api_key,omitempty"`       // 上游密钥
	BalanceURL   string `json:"balance_url,omitempty"`   // 可选的余额查询地址（GET + 本密钥）
}

// MuteWindow 每天重复的静音时段，支持跨零点（如 22:00~06:00）
type MuteWindow struct {
	From string `json:"from"` // "HH:MM"
	To   string `json:"to"`
}

// Model 一条路由规则：某 provider 上的某个模型
type Model struct {
	ID         string       `json:"id"`
	ProviderID string       `json:"provider_id"`
	Name       string       `json:"name"`        // 对外模型名，须与请求体 model 字段一致
	Priority   int          `json:"priority"`    // 越大越优先
	DailyLimit int64        `json:"daily_limit"` // 每日请求上限，0 = 不限
	Mutes      []MuteWindow `json:"mutes,omitempty"`
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

// Store 全部状态：内存为准，变更即落盘
type Store struct {
	mu        sync.Mutex
	path      string
	NextID    int                 `json:"next_id"`
	Providers []*Provider         `json:"providers"`
	Models    []*Model            `json:"models"`
	APIKeys   []*APIKey           `json:"api_keys"`
	Usage     []*UsageStat        `json:"usage"`
	Balances  map[string]*Balance `json:"balances"`
	Logs      []*LogEntry         `json:"logs,omitempty"`       // 异常/失败日志，最多 maxLogs 条
	AutoModel string              `json:"auto_model,omitempty"` // 对外声明的自动模型名，空 = "auto"
}

func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, Balances: map[string]*Balance{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	s.path = path
	if s.Balances == nil {
		s.Balances = map[string]*Balance{}
	}
	return s, nil
}

// save 原子落盘，并裁掉 35 天前的用量（调用方需持锁）
func (s *Store) save() {
	if s.path == "" {
		return
	}
	cut := time.Now().AddDate(0, 0, -35).Format("2006-01-02")
	kept := s.Usage[:0]
	for _, u := range s.Usage {
		if u.Date >= cut {
			kept = append(kept, u)
		}
	}
	s.Usage = kept
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.path+".tmp", b, 0o644); err != nil {
		return
	}
	os.Rename(s.path+".tmp", s.path)
}

func (s *Store) newID(prefix string) string {
	s.NextID++
	return fmt.Sprintf("%s%d", prefix, s.NextID)
}

// Provider 返回 provider 的拷贝
func (s *Store) Provider(id string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Providers {
		if p.ID == id {
			return *p, true
		}
	}
	return Provider{}, false
}

// UpsertProvider ID 为空则新建，否则按 ID 替换
func (s *Store) UpsertProvider(p *Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.Providers {
		if old.ID == p.ID {
			s.Providers[i] = p
			s.save()
			return
		}
	}
	if p.ID == "" {
		p.ID = s.newID("p")
	}
	s.Providers = append(s.Providers, p)
	s.save()
}

// DeleteProvider 连同其模型、用量、余额记录一起删
func (s *Store) DeleteProvider(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.Providers {
		if p.ID != id {
			s.Providers[n] = p
			n++
		}
	}
	if n == len(s.Providers) {
		return false
	}
	s.Providers = s.Providers[:n]
	m := 0
	for _, x := range s.Models {
		if x.ProviderID != id {
			s.Models[m] = x
			m++
		}
	}
	s.Models = s.Models[:m]
	u := 0
	for _, x := range s.Usage {
		if x.ProviderID != id {
			s.Usage[u] = x
			u++
		}
	}
	s.Usage = s.Usage[:u]
	delete(s.Balances, id)
	s.save()
	return true
}

// Model 返回模型条目的拷贝
func (s *Store) Model(id string) (Model, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID == id {
			return *m, true
		}
	}
	return Model{}, false
}

func (s *Store) UpsertModel(m *Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.Models {
		if old.ID == m.ID {
			s.Models[i] = m
			s.save()
			return
		}
	}
	if m.ID == "" {
		m.ID = s.newID("m")
	}
	s.Models = append(s.Models, m)
	s.save()
}

func (s *Store) DeleteModel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.Models {
		if m.ID != id {
			s.Models[n] = m
			n++
		}
	}
	if n == len(s.Models) {
		return false
	}
	s.Models = s.Models[:n]
	s.save()
	return true
}

// Reorder 按传入顺序重新赋优先级，排最前者最大
func (s *Store) Reorder(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		for i, id := range ids {
			if m.ID == id {
				m.Priority = len(ids) - i
				break
			}
		}
	}
	s.save()
}

// AddAPIKey 生成并保存一个客户端密钥
func (s *Store) AddAPIKey(name string) *APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &APIKey{ID: s.newID("k"), Name: name, Key: genAPIKey(), CreatedAt: time.Now()}
	s.APIKeys = append(s.APIKeys, k)
	s.save()
	return k
}

func (s *Store) DeleteAPIKey(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.APIKeys {
		if k.ID != id {
			s.APIKeys[n] = k
			n++
		}
	}
	if n == len(s.APIKeys) {
		return false
	}
	s.APIKeys = s.APIKeys[:n]
	s.save()
	return true
}

// MatchAPIKey 常数时间比较，命中返回 key ID
func (s *Store) MatchAPIKey(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.APIKeys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k.Key)) == 1 {
			return k.ID, true
		}
	}
	return "", false
}

func (s *Store) HasAPIKeys() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.APIKeys) > 0
}

func genAPIKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "sk-r-" + hex.EncodeToString(b)
}

// Usage 一次调用的 token 用量
type Usage struct {
	Prompt     int64 // 输入总 tokens（含缓存命中部分）
	Cached     int64 // 其中命中缓存的 tokens
	Completion int64 // 输出 tokens
}

// AddUsage 记一次调用并落盘
func (s *Store) AddUsage(m *Model, keyID string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	for _, st := range s.Usage {
		if st.Date == today && st.KeyID == keyID && st.ProviderID == m.ProviderID && st.Model == m.Name {
			st.Requests++
			st.PromptTokens += u.Prompt
			st.CachedTokens += u.Cached
			st.CompletionTokens += u.Completion
			s.save()
			return
		}
	}
	s.Usage = append(s.Usage, &UsageStat{
		Date: today, KeyID: keyID, ProviderID: m.ProviderID, Model: m.Name,
		Requests: 1, PromptTokens: u.Prompt, CachedTokens: u.Cached, CompletionTokens: u.Completion,
	})
	s.save()
}

func (s *Store) SetBalance(pid string, b *Balance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Balances[pid] = b
	s.save()
}

// Candidate 参与本次路由的一个候选（拷贝，避免和后台编辑竞态）
type Candidate struct {
	Provider Provider
	Model    Model
}

// AutoModelName 对外声明的自动模型名（默认 auto）
func (s *Store) AutoModelName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AutoModel == "" {
		return "auto"
	}
	return s.AutoModel
}

func (s *Store) SetAutoModel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AutoModel = name
	s.save()
}

// CandidateByID 按模型条目 ID 取候选，不做静音/上限过滤（会话绑定直连用）
func (s *Store) CandidateByID(modelID string) (Candidate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID != modelID {
			continue
		}
		for _, p := range s.Providers {
			if p.ID == m.ProviderID {
				c := Candidate{Provider: *p, Model: *m}
				c.Model.Mutes = append([]MuteWindow(nil), m.Mutes...)
				return c, true
			}
		}
		return Candidate{}, false
	}
	return Candidate{}, false
}

// AllCandidates 所有模型的可用候选（auto 虚拟模型路由用），剔除静音中、超每日上限的，按优先级降序
func (s *Store) AllCandidates(now time.Time) []Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := now.Format("2006-01-02")
	var out []Candidate
	for _, m := range s.Models {
		if mutedNow(m.Mutes, now) || s.overLimit(m, today) {
			continue
		}
		for _, p := range s.Providers {
			if p.ID == m.ProviderID {
				c := Candidate{Provider: *p, Model: *m}
				c.Model.Mutes = append([]MuteWindow(nil), m.Mutes...)
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.Priority > out[j].Model.Priority })
	return out
}

// Candidates 找出该名字的可用候选：剔除静音中、今日 token 已达上限的，按优先级降序
func (s *Store) Candidates(name string, now time.Time) []Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := now.Format("2006-01-02")
	var out []Candidate
	for _, m := range s.Models {
		if m.Name != name || mutedNow(m.Mutes, now) || s.overLimit(m, today) {
			continue
		}
		for _, p := range s.Providers {
			if p.ID == m.ProviderID {
				c := Candidate{Provider: *p, Model: *m}
				c.Model.Mutes = append([]MuteWindow(nil), m.Mutes...)
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.Priority > out[j].Model.Priority })
	return out
}

// overLimit 今日 token 用量（输入+输出）是否已达上限（调用方需持锁）
func (s *Store) overLimit(m *Model, today string) bool {
	if m.DailyLimit <= 0 {
		return false
	}
	for _, u := range s.Usage {
		if u.Date == today && u.ProviderID == m.ProviderID && u.Model == m.Name {
			return u.PromptTokens+u.CompletionTokens >= m.DailyLimit
		}
	}
	return false
}

func mutedNow(ws []MuteWindow, now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()
	for _, w := range ws {
		f, err1 := time.Parse("15:04", w.From)
		t, err2 := time.Parse("15:04", w.To)
		if err1 != nil || err2 != nil || f.Equal(t) {
			continue
		}
		fm, tm := f.Hour()*60+f.Minute(), t.Hour()*60+t.Minute()
		if fm < tm {
			if cur >= fm && cur < tm {
				return true
			}
		} else if cur >= fm || cur < tm { // 跨零点时段
			return true
		}
	}
	return false
}

// ModelNames 去重后的对外模型名列表
func (s *Store) ModelNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, m := range s.Models {
		set[m.Name] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
