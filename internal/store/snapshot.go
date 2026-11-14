package store

import (
	"encoding/json"
	"sort"
	"time"
)

// SnapshotJSON 持锁快照并序列化，避免和后台编辑、用量记账竞态
func (s *Store) SnapshotJSON() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	used := map[string]int64{}
	for _, u := range s.Usage {
		if u.Date == today {
			used[u.ProviderID+"\x00"+u.Model] += u.PromptTokens + u.CompletionTokens
		}
	}
	// 空状态也要输出 [] 而非 null，前端才不会拿到 null 数组
	v := StateView{Providers: []*Provider{}, Groups: []*RouteGroup{}, Models: []ModelView{}, APIKeys: []*APIKey{}, Usage: []*UsageStat{}, SessionUsage: []*SessionStat{}, Balances: map[string]*Balance{}, Logs: []*LogEntry{}}
	if s.Providers != nil {
		v.Providers = s.Providers
	}
	if s.Groups != nil {
		v.Groups = s.Groups
	}
	if s.APIKeys != nil {
		v.APIKeys = s.APIKeys
	}
	if s.Balances != nil {
		v.Balances = s.Balances
	}
	if s.Logs != nil {
		v.Logs = s.Logs
	}
	for _, m := range s.Models {
		external := m.Name
		if p, ok := s.providerLocked(m.ProviderID); ok {
			external = externalModelName(p, m)
		}
		v.Models = append(v.Models, ModelView{Model: m, ExternalName: external, UsedToday: used[m.ProviderID+"\x00"+m.Name], Inflight: s.inflight[m.ID]})
	}
	v.Usage = append(v.Usage, s.Usage...)
	v.SessionUsage = append(v.SessionUsage, s.SessionUsage...)
	v.AutoModel = s.AutoModel // 已持锁，直接读；空值由前端按默认 auto 处理
	v.ResponsesConvert = s.ResponsesConvert
	v.SessionTTLSeconds = int64(s.sessionTTLLocked() / time.Second)
	sort.Slice(v.Usage, func(i, j int) bool {
		a, b := v.Usage[i], v.Usage[j]
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		return a.Model < b.Model
	})
	sort.Slice(v.SessionUsage, func(i, j int) bool {
		a, b := v.SessionUsage[i], v.SessionUsage[j]
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		return a.LastAt.After(b.LastAt)
	})
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
