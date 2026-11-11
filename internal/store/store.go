package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
)

// 本包内部沿用领域模型的短名称，让 CRUD 实现聚焦在持久化逻辑上。
type (
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

// Store 全部状态：内存为准，变更即落盘
type Store struct {
	mu        sync.Mutex
	path      string
	NextID    int          `json:"next_id"`
	Providers []*Provider  `json:"providers"`
	Models    []*Model     `json:"models"`
	APIKeys   []*APIKey    `json:"api_keys"`
	Usage     []*UsageStat `json:"usage"`
	// SessionUsage 按天 × 会话 聚合的用量；会话过期后仍保留，供用量页按会话查看。
	SessionUsage []*SessionStat      `json:"session_usage,omitempty"`
	Balances     map[string]*Balance `json:"balances"`
	Logs         []*LogEntry         `json:"logs,omitempty"` // 异常/失败日志，最多 maxLogs 条

	// Groups 路由组：每个组对外暴露一个虚拟模型名，只在该组的模型里选路。
	// 至少保留一个；第一组是默认组（兼容旧的单一 auto 语义）。
	Groups []*RouteGroup `json:"groups,omitempty"`
	// AutoModel 旧版单一自动模型名，仅用于读取旧状态文件做迁移；
	// 加载后始终与第一组名字同步，实际路由以 Groups 为准。
	AutoModel string `json:"auto_model,omitempty"`

	// ResponsesConvert 是否允许把 /v1/responses 请求翻译成 chat/completions 出站。
	// 默认关闭：仅支持 chat 的上游（WorkBuddy）不承接 responses 请求，路由会改选
	// 其他原生支持 responses 的模型；开启后此类模型才参与 responses 路由（由适配层转换）。
	ResponsesConvert bool `json:"responses_convert,omitempty"`

	// SessionTTLSeconds 会话粘性的滑动过期秒数，0 = 默认（sessionTTL，1 小时）。
	// 每次会话内成功请求都会重新计时；通过设置接口修改时，现有绑定会立即按新值重算。
	SessionTTLSeconds int64 `json:"session_ttl_seconds,omitempty"`

	// SchemaVersion 状态文件结构版本，用于后续迁移；加载旧文件时补写。
	SchemaVersion int `json:"schema_version,omitempty"`

	// onError 落盘失败回调（由组合根注入日志），避免静默丢数据。
	onError func(error)

	// async 热数据（用量/会话统计/日志）是否合并落盘；配置变更始终同步落盘。
	async    bool
	hotDirty bool
	hotTimer *time.Timer

	// inflight 各模型条目当前在途请求数，仅内存、不落盘（重启归零）。
	// 键是模型条目 ID，这样同一模型名挂在多个 Provider 上时各自独立计数。
	inflight map[string]int
}

func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, Balances: map[string]*Balance{}}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, s); err != nil {
			return nil, err
		}
	}
	s.path = path
	s.SchemaVersion = schemaVersion
	// 热数据独立文件；旧版 data.json 里的用量仍兼容读取。
	if hb, err := os.ReadFile(path + ".usage"); err == nil {
		var h hotState
		if err := json.Unmarshal(hb, &h); err != nil {
			return nil, err
		}
		s.Usage, s.SessionUsage, s.Logs = h.Usage, h.SessionUsage, h.Logs
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.Balances == nil {
		s.Balances = map[string]*Balance{}
	}
	s.normalizeGroupsLocked() // 旧状态文件没有组：按原 auto_model 建默认组，模型全部归入
	return s, nil
}

// normalizeGroupsLocked 修复/迁移路由组数据：至少保留一个组；把旧版「模型归属单个组」
// 迁移为「组持有成员列表」；每条模型配置至少加入默认组；并重算直连优先级。
// 调用方需持锁（LoadStore 为单线程，可直接调用）。
func (s *Store) normalizeGroupsLocked() {
	seen := make(map[string]bool, len(s.Groups))
	groups := s.Groups[:0]
	for _, g := range s.Groups {
		if g == nil || g.ID == "" || seen[g.ID] {
			continue
		}
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			continue
		}
		seen[g.ID] = true
		if g.Members == nil {
			g.Members = []GroupMember{} // JSON 输出 [] 而不是 null
		}
		groups = append(groups, g)
	}
	s.Groups = groups
	if len(s.Groups) == 0 {
		name := strings.TrimSpace(s.AutoModel)
		if name == "" {
			name = "auto"
		}
		s.Groups = []*RouteGroup{{ID: domain.DefaultGroupID, Name: name, Members: []GroupMember{}}}
	}

	models := make(map[string]*Model, len(s.Models))
	for _, m := range s.Models {
		if m != nil && m.ID != "" {
			models[m.ID] = m
		}
	}
	// 旧版数据：模型自带 group_id，折叠成目标组的成员关系（保留组内优先级）。
	for _, m := range s.Models {
		if m == nil {
			continue
		}
		if m.GroupID != "" {
			if g := s.groupByIDLocked(m.GroupID); g != nil {
				s.addMemberLocked(g, m.ID, m.Priority)
			}
			m.GroupID = ""
		}
	}
	// 清理悬空/重复成员，并记下已有归属的模型。
	memberOf := make(map[string]bool, len(models))
	for _, g := range s.Groups {
		out := g.Members[:0]
		seenMember := make(map[string]bool, len(g.Members))
		for _, mem := range g.Members {
			if mem.ModelID == "" || seenMember[mem.ModelID] {
				continue
			}
			if _, ok := models[mem.ModelID]; !ok {
				continue
			}
			seenMember[mem.ModelID] = true
			memberOf[mem.ModelID] = true
			out = append(out, mem)
		}
		g.Members = out
	}
	// 每条模型配置至少属于一个组（兼容旧数据与直接 UpsertModel 的场景）。
	def := s.Groups[0]
	for _, m := range s.Models {
		if m != nil && m.ID != "" && !memberOf[m.ID] {
			s.addMemberLocked(def, m.ID, m.Priority)
		}
	}
	s.recomputeModelPrioritiesLocked()
	s.AutoModel = def.Name
}

// groupByIDLocked 按 ID 取组指针（调用方需持锁）
func (s *Store) groupByIDLocked(id string) *RouteGroup {
	for _, g := range s.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// memberIndexLocked 成员在组内的下标，-1 = 不是成员（调用方需持锁）
func memberIndexLocked(g *RouteGroup, modelID string) int {
	for i, mem := range g.Members {
		if mem.ModelID == modelID {
			return i
		}
	}
	return -1
}

// addMemberLocked 加入成员；已存在则保留原组内优先级（调用方需持锁）
func (s *Store) addMemberLocked(g *RouteGroup, modelID string, priority int) {
	if g == nil || modelID == "" || memberIndexLocked(g, modelID) >= 0 {
		return
	}
	g.Members = append(g.Members, GroupMember{ModelID: modelID, Priority: priority})
}

// setMemberPriorityLocked 加入成员或更新其在组内的优先级（调用方需持锁）
func (s *Store) setMemberPriorityLocked(g *RouteGroup, modelID string, priority int) {
	if g == nil || modelID == "" {
		return
	}
	if i := memberIndexLocked(g, modelID); i >= 0 {
		g.Members[i].Priority = priority
		return
	}
	g.Members = append(g.Members, GroupMember{ModelID: modelID, Priority: priority})
}

// anyMembershipLocked 该模型是否仍属于至少一个组（调用方需持锁）
func (s *Store) anyMembershipLocked(modelID string) bool {
	for _, g := range s.Groups {
		if memberIndexLocked(g, modelID) >= 0 {
			return true
		}
	}
	return false
}

// recomputeModelPriorityLocked 直连优先级 = 所有成员优先级的最大值（调用方需持锁）
func (s *Store) recomputeModelPriorityLocked(m *Model) {
	if m == nil {
		return
	}
	best, found := 0, false
	for _, g := range s.Groups {
		if i := memberIndexLocked(g, m.ID); i >= 0 {
			if !found || g.Members[i].Priority > best {
				best, found = g.Members[i].Priority, true
			}
		}
	}
	if found {
		m.Priority = best
	} else {
		m.Priority = 0
	}
}

// recomputeModelPrioritiesLocked 重算全部模型的直连优先级（调用方需持锁）
func (s *Store) recomputeModelPrioritiesLocked() {
	for _, m := range s.Models {
		s.recomputeModelPriorityLocked(m)
	}
}

// configFile data.json 的持久化形状：只含配置（冷数据），不含高频用量。
type configFile struct {
	SchemaVersion     int                 `json:"schema_version,omitempty"`
	NextID            int                 `json:"next_id"`
	Providers         []*Provider         `json:"providers"`
	Models            []*Model            `json:"models"`
	APIKeys           []*APIKey           `json:"api_keys"`
	Balances          map[string]*Balance `json:"balances"`
	Groups            []*RouteGroup       `json:"groups,omitempty"`
	AutoModel         string              `json:"auto_model,omitempty"`
	ResponsesConvert  bool                `json:"responses_convert,omitempty"`
	SessionTTLSeconds int64               `json:"session_ttl_seconds,omitempty"`
}

// hotState 用量文件（data.json.usage）的形状：高频写、可合并落盘。
type hotState struct {
	Usage        []*UsageStat   `json:"usage,omitempty"`
	SessionUsage []*SessionStat `json:"session_usage,omitempty"`
	Logs         []*LogEntry    `json:"logs,omitempty"`
}

// hotFlushDelay 热数据合并落盘的窗口；崩溃最多丢失该窗口内的用量。
const hotFlushDelay = 2 * time.Second

// save 同步落盘配置（调用方需持锁）。用量/日志走 saveHot。
func (s *Store) save() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(configFile{
		SchemaVersion:     s.SchemaVersion,
		NextID:            s.NextID,
		Providers:         s.Providers,
		Models:            s.Models,
		APIKeys:           s.APIKeys,
		Balances:          s.Balances,
		Groups:            s.Groups,
		AutoModel:         s.AutoModel,
		ResponsesConvert:  s.ResponsesConvert,
		SessionTTLSeconds: s.SessionTTLSeconds,
	}, "", "  ")
	if err != nil {
		s.reportSaveError(err)
		return
	}
	s.writeAtomic(s.path, b)
}

// saveHot 记录一次热数据变更（调用方需持锁）。
// 测试/未开启异步时同步写；生产环境合并到 1 秒窗口。
func (s *Store) saveHot() {
	if s.path == "" {
		return
	}
	if !s.async {
		b, err := s.marshalHotLocked()
		if err != nil {
			s.reportSaveError(err)
			return
		}
		s.writeAtomic(s.hotPath(), b)
		return
	}
	s.hotDirty = true
	if s.hotTimer == nil {
		s.hotTimer = time.AfterFunc(hotFlushDelay, s.flushHot)
	}
}

// marshalHotLocked 裁剪 35 天前的用量后序列化（调用方需持锁）。
func (s *Store) marshalHotLocked() ([]byte, error) {
	cut := time.Now().AddDate(0, 0, -35).Format("2006-01-02")
	usage := s.Usage[:0]
	for _, u := range s.Usage {
		if u.Date >= cut {
			usage = append(usage, u)
		}
	}
	s.Usage = usage
	sess := s.SessionUsage[:0]
	for _, u := range s.SessionUsage {
		if u.Date >= cut {
			sess = append(sess, u)
		}
	}
	s.SessionUsage = sess
	return json.MarshalIndent(hotState{Usage: s.Usage, SessionUsage: s.SessionUsage, Logs: s.Logs}, "", "  ")
}

// flushHot 合并窗口到期或进程退出时落盘热数据。
func (s *Store) flushHot() {
	s.mu.Lock()
	s.hotTimer = nil
	if !s.hotDirty {
		s.mu.Unlock()
		return
	}
	s.hotDirty = false
	b, err := s.marshalHotLocked()
	s.mu.Unlock()
	if err != nil {
		s.reportSaveError(err)
		return
	}
	s.writeAtomic(s.hotPath(), b)
}

// Flush 立即落盘待写热数据（优雅退出/升级前调用）。
func (s *Store) Flush() {
	s.mu.Lock()
	if s.hotTimer != nil {
		s.hotTimer.Stop()
		s.hotTimer = nil
	}
	if !s.hotDirty {
		s.mu.Unlock()
		return
	}
	s.hotDirty = false
	b, err := s.marshalHotLocked()
	s.mu.Unlock()
	if err != nil {
		s.reportSaveError(err)
		return
	}
	s.writeAtomic(s.hotPath(), b)
}

// SetAsyncWrites 开启/关闭热数据合并落盘（生产开启，测试默认同步）。
func (s *Store) SetAsyncWrites(v bool) { s.async = v }

// hotPath 用量文件路径。
func (s *Store) hotPath() string { return s.path + ".usage" }

// writeAtomic 原子写文件（0600：内含凭证/密钥/用量）。
func (s *Store) writeAtomic(path string, b []byte) {
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		s.reportSaveError(err)
		return
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		s.reportSaveError(err)
	}
}

// schemaVersion 当前状态文件结构版本。
const schemaVersion = 2

func (s *Store) reportSaveError(err error) {
	if s.onError != nil {
		s.onError(err)
	}
}

// SetSaveErrorHandler 注入落盘失败回调（组合根用于写日志）。
func (s *Store) SetSaveErrorHandler(fn func(error)) { s.onError = fn }

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

// UpdateWBAuth 回写 WorkBuddy 续期后的凭证（refresh token 一次性，轮换后旧值即失效，必须落盘）。
// 整体替换 WB 指针而非原地改字段：Candidate 里的 Provider 是浅拷贝，共享同一 WB 指针，
// 换指针可让并发请求各自持有不可变的旧快照，不产生数据竞争。
func (s *Store) UpdateWBAuth(providerID string, c *WorkBuddyCred) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.WB == nil {
		return false
	}
	cp := *c
	p.WB = &cp
	s.save()
	return true
}

// UpdateWBUsage 回写 WorkBuddy 余额快照。与 UpdateWBAuth 同样走换指针拷贝：
// 从当前凭证复制后仅改 Usage，不覆盖并发写入的新 token。
func (s *Store) UpdateWBUsage(providerID string, u *WBUsage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.WB == nil {
		return false
	}
	cp := *p.WB
	cp.Usage = u
	p.WB = &cp
	s.save()
	return true
}

// UpdateWBCheckinDay 记录当天已签到（本地日期 2006-01-02），同日不再重复签到。
// 与 UpdateWBUsage 同样从当前凭证复制后仅改这一个字段，不覆盖并发写入的新 token。
func (s *Store) UpdateWBCheckinDay(providerID, day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.WB == nil {
		return false
	}
	cp := *p.WB
	cp.LastCheckinDay = day
	p.WB = &cp
	s.save()
	return true
}

// UpdateZedAuth 回写 Zed 重铸后的凭证（换指针拷贝，理由同 UpdateWBAuth：
// Candidate 里的 Provider 是浅拷贝，并发请求各自持有不可变旧快照）。
func (s *Store) UpdateZedAuth(providerID string, c *ZedCred) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.Zed == nil {
		return false
	}
	cp := *c
	p.Zed = &cp
	s.save()
	return true
}

// UpdateCommandCodeAuth 回写 Command Code 刷新后的凭证（换指针拷贝，理由同
// UpdateWBAuth：Candidate 里的 Provider 是浅拷贝，并发请求各自持有不可变快照）。
func (s *Store) UpdateCommandCodeAuth(providerID string, c *CommandCodeCred) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.CommandCode == nil {
		return false
	}
	cp := *c
	p.CommandCode = &cp
	s.save()
	return true
}

// ProviderByCommandCodeKey 按 API Key 找 provider（重复导入时原地更新）
func (s *Store) ProviderByCommandCodeKey(key string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Providers {
		if p.Kind == "commandcode" && p.CommandCode != nil && p.CommandCode.APIKey == key {
			return *p, true
		}
	}
	return Provider{}, false
}

// UpdateZedPlan 回写 Zed 账号/套餐快照。从当前凭证复制后仅改 Plan，
// 不覆盖并发写入的新 token。
func (s *Store) UpdateZedPlan(providerID string, plan *ZedPlan) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerLocked(providerID)
	if !ok || p.Zed == nil {
		return false
	}
	cp := *p.Zed
	cp.Plan = plan
	p.Zed = &cp
	s.save()
	return true
}

// AllProviders 全量 Provider 浅拷贝（后台任务遍历用，元素只读）
func (s *Store) AllProviders() []Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Provider, 0, len(s.Providers))
	for _, p := range s.Providers {
		out = append(out, *p)
	}
	return out
}

// ProviderByWBUID 按 Kind + WorkBuddy 账号 uid 找 provider（凭证重复导入时原地更新）
func (s *Store) ProviderByWBUID(kind, uid string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Providers {
		if p.Kind == kind && p.WB != nil && p.WB.UID == uid {
			return *p, true
		}
	}
	return Provider{}, false
}

// ProviderByZedUser 按 Zed 账号 user_id 找 provider（重复登录时原地更新）
func (s *Store) ProviderByZedUser(userID string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Providers {
		if p.Kind == "zed" && p.Zed != nil && p.Zed.UserID == userID {
			return *p, true
		}
	}
	return Provider{}, false
}

// HasZedModel 判断某 provider 下是否已有同名模型条目（登录同步模型时去重）
func (s *Store) HasZedModel(providerID, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ProviderID == providerID && m.Name == name {
			return true
		}
	}
	return false
}

// ProviderModelCount 某 provider 名下的模型条目数（后台展示用）
func (s *Store) ProviderModelCount(providerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.Models {
		if m.ProviderID == providerID {
			n++
		}
	}
	return n
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
	removed := map[string]bool{}
	m := 0
	for _, x := range s.Models {
		if x.ProviderID != id {
			s.Models[m] = x
			m++
			continue
		}
		removed[x.ID] = true
	}
	s.Models = s.Models[:m]
	for _, g := range s.Groups { // 清掉指向已删模型的成员关系
		kept := g.Members[:0]
		for _, mem := range g.Members {
			if !removed[mem.ModelID] {
				kept = append(kept, mem)
			}
		}
		g.Members = kept
	}
	u := 0
	for _, x := range s.Usage {
		if x.ProviderID != id {
			s.Usage[u] = x
			u++
		}
	}
	s.Usage = s.Usage[:u]
	su := 0
	for _, x := range s.SessionUsage {
		if x.ProviderID != id {
			s.SessionUsage[su] = x
			su++
		}
	}
	s.SessionUsage = s.SessionUsage[:su]
	delete(s.Balances, id)
	s.save()
	s.saveHot() // 用量/会话统计也随 Provider 一起删除
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

// UpsertModel 保存共享模型配置。新模型至少加入默认组；已有模型保留其全部组成员关系。
func (s *Store) UpsertModel(m *Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertModelLocked(m, "")
	s.save()
}

// UpsertModelInGroup 保存共享模型配置，并设置它在 targetGroupID 组内的优先级。
// 同一个模型配置可被多个组引用；这里只改目标组的成员优先级，不影响其他组。
func (s *Store) UpsertModelInGroup(m *Model, groupID string, priority int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if groupID == "" {
		groupID = s.defaultGroupIDLocked()
	}
	g := s.groupByIDLocked(groupID)
	if g == nil {
		return fmt.Errorf("路由组不存在")
	}
	s.upsertModelLocked(m, g.ID)
	s.setMemberPriorityLocked(g, m.ID, priority)
	s.recomputeModelPriorityLocked(m)
	s.save()
	return nil
}

// upsertModelLocked 保存配置（调用方需持锁）。targetGroupID 非空时确保模型加入该组；
// 否则新模型加入默认组。旧版携带的 GroupID 也在此迁移后清空。
func (s *Store) upsertModelLocked(m *Model, targetGroupID string) {
	legacyGroupID := m.GroupID
	m.GroupID = ""
	for i, old := range s.Models {
		if old.ID != "" && old.ID == m.ID {
			// 后台弹窗不提交运行时静音状态，更新时保留，避免一次编辑把限流静音清掉
			m.MutedUntil = old.MutedUntil
			m.MuteReason = old.MuteReason
			s.Models[i] = m
			s.recomputeModelPriorityLocked(m)
			return
		}
	}
	if m.ID == "" {
		m.ID = s.newID("m")
	}
	s.Models = append(s.Models, m)
	gid := targetGroupID
	if gid == "" {
		gid = legacyGroupID
	}
	if g := s.groupByIDLocked(gid); g != nil {
		s.addMemberLocked(g, m.ID, m.Priority)
	} else if g := s.groupByIDLocked(s.defaultGroupIDLocked()); g != nil {
		s.addMemberLocked(g, m.ID, m.Priority)
	}
	s.recomputeModelPriorityLocked(m)
}

// DeleteModel 删除共享模型配置，并从所有路由组中移除该成员。
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
	for _, g := range s.Groups {
		kept := g.Members[:0]
		for _, mem := range g.Members {
			if mem.ModelID != id {
				kept = append(kept, mem)
			}
		}
		g.Members = kept
	}
	s.save()
	return true
}

// ReorderGroup 按传入顺序重排某组的成员优先级，排最前者最大；只影响该组。
func (s *Store) ReorderGroup(groupID string, ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupByIDLocked(groupID)
	if g == nil {
		return false
	}
	for i, id := range ids {
		if idx := memberIndexLocked(g, id); idx >= 0 {
			g.Members[idx].Priority = len(ids) - i
		}
	}
	s.recomputeModelPrioritiesLocked()
	s.save()
	return true
}

// Reorder 兼容旧接口：重排默认组成员。
func (s *Store) Reorder(ids []string) {
	s.ReorderGroup(s.DefaultGroupID(), ids)
}

// AttachModelToGroup 把已有模型配置加入组（已在组内则更新组内优先级）。
func (s *Store) AttachModelToGroup(groupID, modelID string, priority int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupByIDLocked(groupID)
	if g == nil {
		return fmt.Errorf("路由组不存在")
	}
	m := s.modelByIDLocked(modelID)
	if m == nil {
		return fmt.Errorf("模型不存在")
	}
	s.setMemberPriorityLocked(g, modelID, priority)
	s.recomputeModelPriorityLocked(m)
	s.save()
	return nil
}

// DetachModelFromGroup 只把模型移出该组；若它已不属于任何组，则连同共享配置一起删除。
func (s *Store) DetachModelFromGroup(groupID, modelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupByIDLocked(groupID)
	if g == nil {
		return false
	}
	i := memberIndexLocked(g, modelID)
	if i < 0 {
		return false
	}
	g.Members = append(g.Members[:i], g.Members[i+1:]...)
	if m := s.modelByIDLocked(modelID); m != nil {
		if !s.anyMembershipLocked(modelID) {
			n := 0
			for _, x := range s.Models {
				if x.ID != modelID {
					s.Models[n] = x
					n++
				}
			}
			s.Models = s.Models[:n]
		} else {
			s.recomputeModelPriorityLocked(m)
		}
	}
	s.save()
	return true
}

// modelByIDLocked 按 ID 取模型指针（调用方需持锁）
func (s *Store) modelByIDLocked(id string) *Model {
	for _, m := range s.Models {
		if m.ID == id {
			return m
		}
	}
	return nil
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
// AddUsage 记一次调用并落盘（全局按天聚合）
func (s *Store) AddUsage(m *Model, keyID string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addUsageLocked(m, keyID, u)
	s.saveHot()
}

// addUsageLocked 更新「按天 × key × provider × 模型」的全局用量（调用方需持锁，不落盘）
func (s *Store) addUsageLocked(m *Model, keyID string, u Usage) {
	today := time.Now().Format("2006-01-02")
	for _, st := range s.Usage {
		if st.Date == today && st.KeyID == keyID && st.ProviderID == m.ProviderID && st.Model == m.Name {
			st.Requests++
			st.PromptTokens += u.Prompt
			st.CachedTokens += u.Cached
			st.CompletionTokens += u.Completion
			return
		}
	}
	s.Usage = append(s.Usage, &UsageStat{
		Date: today, KeyID: keyID, ProviderID: m.ProviderID, Model: m.Name,
		Requests: 1, PromptTokens: u.Prompt, CachedTokens: u.Cached, CompletionTokens: u.Completion,
	})
}

// AddCallUsage 记一次成功调用：全局按天用量 + （带会话 ID 时）按天会话用量。
// 两者一次落盘，避免同一请求写两次状态文件。计时必须来自同一次调用。
func (s *Store) AddCallUsage(sessionID, requested string, m *Model, keyID string, us usageSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addUsageLocked(m, keyID, us.Usage)
	if sessionID != "" {
		s.addSessionStatLocked(sessionID, requested, m, keyID, us)
	}
	s.saveHot()
}

// addSessionStatLocked 更新「按天 × 会话 × key × provider × 模型」的会话用量
// （调用方需持锁，不落盘）。会话过期后这行仍保留，直到 35 天用量清理。
func (s *Store) addSessionStatLocked(sessionID, requested string, m *Model, keyID string, us usageSample) {
	today := time.Now().Format("2006-01-02")
	now := time.Now()
	for _, st := range s.SessionUsage {
		if st.Date == today && st.SessionID == sessionID && st.KeyID == keyID &&
			st.ProviderID == m.ProviderID && st.Model == m.Name && st.Requested == requested {
			st.Requests++
			st.PromptTokens += us.Prompt
			st.CachedTokens += us.Cached
			st.CompletionTokens += us.Completion
			if us.Completion > 0 {
				st.GenNanos += int64(us.Gen)
			}
			if us.Stream && us.TTFT > 0 {
				st.TTFTNanos += int64(us.TTFT)
				st.TTFTSamples++
			}
			st.LastAt = now
			st.EffortDeclared, st.EffortSent = us.EffortDeclared, us.EffortSent
			return
		}
	}
	st := &SessionStat{
		Date: today, SessionID: sessionID, KeyID: keyID,
		ProviderID: m.ProviderID, Model: m.Name, Requested: requested,
		Requests: 1, PromptTokens: us.Prompt, CachedTokens: us.Cached, CompletionTokens: us.Completion,
		FirstAt: now, LastAt: now,
		EffortDeclared: us.EffortDeclared, EffortSent: us.EffortSent,
	}
	if us.Completion > 0 {
		st.GenNanos = int64(us.Gen)
	}
	if us.Stream && us.TTFT > 0 {
		st.TTFTNanos = int64(us.TTFT)
		st.TTFTSamples = 1
	}
	s.SessionUsage = append(s.SessionUsage, st)
}

func (s *Store) SetBalance(pid string, b *Balance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Balances[pid] = b
	s.save()
}

// TryAcquire 尝试占用一个并行名额。成功返回 true，调用方必须配对调用 Release。
// 判断与自增在同一把锁内完成，避免并发请求同时看到"还有空位"而超发。
func (s *Store) TryAcquire(modelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var limit int
	for _, m := range s.Models {
		if m.ID == modelID {
			limit = m.MaxConcurrent
			break
		}
	}
	if limit > 0 && s.inflight[modelID] >= limit {
		return false
	}
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	s.inflight[modelID]++
	return true
}

// Release 释放一个并行名额
func (s *Store) Release(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil {
		return
	}
	if s.inflight[modelID] <= 1 {
		delete(s.inflight, modelID) // 归零即删，避免 map 无限增长
		return
	}
	s.inflight[modelID]--
}

// atCapacity 该模型是否已达并行上限（调用方需持锁）
func (s *Store) atCapacity(m *Model) bool {
	return m.MaxConcurrent > 0 && s.inflight[m.ID] >= m.MaxConcurrent
}

// Inflight 返回各模型当前在途请求数（给后台页面展示）
func (s *Store) Inflight() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.inflight))
	for k, v := range s.inflight {
		out[k] = v
	}
	return out
}

// AutoModelName 默认路由组（第一组）对外声明的模型名（默认 auto）
func (s *Store) AutoModelName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Groups) == 0 {
		return "auto"
	}
	return s.Groups[0].Name
}

// SetAutoModel 重命名默认路由组（兼容旧设置接口），与其他组重名时报错。
func (s *Store) SetAutoModel(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "auto"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Groups) == 0 {
		s.normalizeGroupsLocked()
	}
	for _, g := range s.Groups[1:] {
		if g.Name == name {
			return fmt.Errorf("路由组名 %q 已被其他组使用", name)
		}
	}
	if s.Groups[0].Name == name {
		return nil
	}
	s.Groups[0].Name = name
	s.AutoModel = name
	s.save()
	return nil
}

// cloneGroup 深拷贝组（含成员切片），避免返回值与内部状态共享底层数组
func cloneGroup(g *RouteGroup) RouteGroup {
	c := *g
	c.Members = append([]GroupMember(nil), g.Members...)
	return c
}

// GroupsList 全部路由组的拷贝（后台/模型列表展示用）
func (s *Store) GroupsList() []RouteGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RouteGroup, 0, len(s.Groups))
	for _, g := range s.Groups {
		out = append(out, cloneGroup(g))
	}
	return out
}

// GroupByName 按对外模型名找路由组
func (s *Store) GroupByName(name string) (RouteGroup, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.Groups {
		if g.Name == name {
			return cloneGroup(g), true
		}
	}
	return RouteGroup{}, false
}

// GroupByID 按 ID 找路由组（存在性校验用）
func (s *Store) GroupByID(id string) (RouteGroup, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.groupByIDLocked(id); g != nil {
		return cloneGroup(g), true
	}
	return RouteGroup{}, false
}

// DefaultGroupID 默认（第一）路由组的 ID
func (s *Store) DefaultGroupID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Groups) == 0 {
		return ""
	}
	return s.Groups[0].ID
}

// AddGroup 新建路由组，名字在组间必须唯一。
func (s *Store) AddGroup(name, note string, contextLength int64) (RouteGroup, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RouteGroup{}, fmt.Errorf("路由组名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Groups) == 0 {
		s.normalizeGroupsLocked()
	}
	for _, g := range s.Groups {
		if g.Name == name {
			return RouteGroup{}, fmt.Errorf("路由组名 %q 已存在", name)
		}
	}
	g := &RouteGroup{ID: s.newID("g"), Name: name, Note: strings.TrimSpace(note), Members: []GroupMember{}, ContextLength: max(contextLength, 0)}
	s.Groups = append(s.Groups, g)
	s.save()
	return cloneGroup(g), nil
}

// UpdateGroup 重命名/改备注/改上下文声明；名字同样在组间唯一。
func (s *Store) UpdateGroup(id, name, note string, contextLength int64) (RouteGroup, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RouteGroup{}, fmt.Errorf("路由组名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *RouteGroup
	for _, g := range s.Groups {
		if g.ID != id {
			if g.Name == name {
				return RouteGroup{}, fmt.Errorf("路由组名 %q 已存在", name)
			}
			continue
		}
		target = g
	}
	if target == nil {
		return RouteGroup{}, fmt.Errorf("路由组不存在")
	}
	target.Name = name
	target.Note = strings.TrimSpace(note)
	target.ContextLength = max(contextLength, 0)
	if s.Groups[0] == target {
		s.AutoModel = target.Name
	}
	s.save()
	return cloneGroup(target), nil
}

// DeleteGroup 删除路由组。共享模型配置若仍属于其他组则保留；只删除已无任何归属的模型。
// 至少保留一个路由组。
func (s *Store) DeleteGroup(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Groups) <= 1 {
		return false
	}
	idx := -1
	for i, g := range s.Groups {
		if g.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	s.Groups = append(s.Groups[:idx], s.Groups[idx+1:]...)
	n := 0
	for _, m := range s.Models {
		if s.anyMembershipLocked(m.ID) {
			s.Models[n] = m
			n++
			continue
		}
	}
	s.Models = s.Models[:n] // 仅删掉不再属于任何组的模型配置
	s.recomputeModelPrioritiesLocked()
	s.AutoModel = s.Groups[0].Name
	s.save()
	return true
}

// defaultGroupIDLocked 默认组 ID，必要时先修复组数据（调用方需持锁）
func (s *Store) defaultGroupIDLocked() string {
	if len(s.Groups) == 0 {
		s.normalizeGroupsLocked()
	}
	return s.Groups[0].ID
}

// ResponsesConvertEnabled 是否允许 /v1/responses → chat/completions 协议转换（默认关闭）
func (s *Store) ResponsesConvertEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponsesConvert
}

// SetResponsesConvert 切换 responses 协议转换开关
func (s *Store) SetResponsesConvert(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ResponsesConvert == v {
		return
	}
	s.ResponsesConvert = v
	s.save()
}

// sessionTTLLocked 当前会话 TTL（调用方需持锁）
func (s *Store) sessionTTLLocked() time.Duration {
	switch {
	case s.SessionTTLSeconds <= 0:
		return domain.DefaultSessionTTL
	case s.SessionTTLSeconds > int64(domain.MaxSessionTTL/time.Second):
		return domain.MaxSessionTTL
	}
	return time.Duration(s.SessionTTLSeconds) * time.Second
}

// SessionTTL 当前会话粘性有效期（默认 1 小时，上限 30 天）
func (s *Store) SessionTTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionTTLLocked()
}

// SetSessionTTL 设置会话粘性有效期；<=0 恢复默认，超过上限按 30 天截断
func (s *Store) SetSessionTTL(seconds int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case seconds <= 0:
		seconds = 0
	case seconds > int64(domain.MaxSessionTTL/time.Second):
		seconds = int64(domain.MaxSessionTTL / time.Second)
	}
	if s.SessionTTLSeconds == seconds {
		return
	}
	s.SessionTTLSeconds = seconds
	s.save()
}

// SetModelExpose 切换某条模型路由是否在 /v1/models 中暴露
func (s *Store) SetModelExpose(id string, expose bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID != id {
			continue
		}
		if m.Expose != expose {
			m.Expose = expose
			s.save()
		}
		return true
	}
	return false
}

// CandidateByID 按模型条目 ID 取候选。会话绑定直连时不套用静音时段/每日上限/并行上限
// （绑定即优先），但上游 429 触发的自动静音仍然生效——那代表上游明确限流了，继续打过去没有意义。
func (s *Store) CandidateByID(modelID string) (Candidate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID != modelID {
			continue
		}
		if domain.MutedUntil(m, time.Now()) {
			return Candidate{}, false
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

// skipReason 该条目为何不可用，返回空串表示可用（调用方需持锁）
func (s *Store) skipReason(m *Model, now time.Time, today string) string {
	switch {
	case domain.MutedUntil(m, now):
		return "上游 429 自动静音至 " + m.MutedUntil.Local().Format("01-02 15:04")
	case domain.MutedNow(m.Mutes, now):
		return "处于静音时段"
	case s.overLimit(m, today):
		return "已达每日 token 上限"
	case s.atCapacity(m):
		return fmt.Sprintf("并行已满 %d/%d", s.inflight[m.ID], m.MaxConcurrent)
	}
	return ""
}

// MuteOn429 上游返回 429 时把该模型条目自动静音一段时间。
// 已在静音期内则延长到更晚的时刻，不缩短。返回静音截止时间。
func (s *Store) MuteOn429(modelID string, d time.Duration, reason string) (time.Time, bool) {
	return s.MuteModel(modelID, d, reason)
}

// MuteModel 通用自动静音（429 限流、Zed 套餐不含该模型等场景共用）。
// 语义与 MuteOn429 一致：已在静音期内则延长到更晚的时刻，不缩短。
func (s *Store) MuteModel(modelID string, d time.Duration, reason string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID != modelID {
			continue
		}
		until := time.Now().Add(d)
		if until.After(m.MutedUntil) {
			m.MutedUntil = until
		} else {
			until = m.MutedUntil // 已有更晚的静音，保持不动
		}
		m.MuteReason = reason
		s.save()
		return until, true
	}
	return time.Time{}, false
}

// ClearMute 手动解除某个模型条目的自动静音
func (s *Store) ClearMute(modelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Models {
		if m.ID != modelID {
			continue
		}
		m.MutedUntil = time.Time{}
		m.MuteReason = ""
		s.save()
		return true
	}
	return false
}

// providerLocked 按 ID 取 provider 指针（调用方需持锁）
func (s *Store) providerLocked(id string) (*Provider, bool) {
	for _, p := range s.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return nil, false
}

// CandidatesDetailed 与 Candidates / AllCandidates 同义，但同时返回被剔除条目的原因。
// name 为空或 all 为 true 时不按名字过滤（全局 fallback 用）。
// candidatesDetailedLocked 见 CandidatesDetailed（调用方需持锁）
func (s *Store) CandidatesDetailed(name string, now time.Time, all bool) ([]Candidate, []SkipReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]candidateEntry, 0, len(s.Models))
	for _, m := range s.Models {
		if all || m.Name == name {
			entries = append(entries, candidateEntry{m: m, priority: m.Priority})
		}
	}
	return s.candidatesLocked(entries, now)
}

// GroupCandidatesDetailed 某路由组内的可用候选，按组内成员优先级降序；
// 同时返回被剔除条目的原因。
func (s *Store) GroupCandidatesDetailed(groupID string, now time.Time) ([]Candidate, []SkipReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupByIDLocked(groupID)
	if g == nil {
		return nil, nil
	}
	entries := make([]candidateEntry, 0, len(g.Members))
	for _, mem := range g.Members {
		if m := s.modelByIDLocked(mem.ModelID); m != nil {
			entries = append(entries, candidateEntry{m: m, priority: mem.Priority})
		}
	}
	return s.candidatesLocked(entries, now)
}

// candidateEntry 一次选路的输入：模型配置 + 本次上下文使用的优先级（组内或直连）
type candidateEntry struct {
	m        *Model
	priority int
}

// candidatesLocked 按给定条目剔除不可用项并排序（调用方需持锁）。
func (s *Store) candidatesLocked(entries []candidateEntry, now time.Time) ([]Candidate, []SkipReason) {
	today := now.Format("2006-01-02")
	var out []Candidate
	var skips []SkipReason
	for _, e := range entries {
		m := e.m
		if r := s.skipReason(m, now, today); r != "" {
			skips = append(skips, SkipReason{ModelID: m.ID, Model: m.Name, ProviderID: m.ProviderID, Reason: r})
			continue
		}
		p, ok := s.providerLocked(m.ProviderID)
		if !ok {
			skips = append(skips, SkipReason{ModelID: m.ID, Model: m.Name, ProviderID: m.ProviderID, Reason: "Provider 不存在"})
			continue
		}
		c := Candidate{Provider: *p, Model: *m}
		c.Model.Priority = e.priority // 组内选路使用成员优先级，直连使用模型直连优先级
		c.Model.Mutes = append([]MuteWindow(nil), m.Mutes...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.Priority > out[j].Model.Priority })
	return out, skips
}

// AllCandidates 所有模型的可用候选（全局兜底用），剔除静音中、超每日上限的，按优先级降序
func (s *Store) AllCandidates(now time.Time) []Candidate {
	out, _ := s.CandidatesDetailed("", now, true)
	return out
}

// Candidates 找出该名字的可用候选：剔除静音中、今日 token 已达上限的，按优先级降序
func (s *Store) Candidates(name string, now time.Time) []Candidate {
	out, _ := s.CandidatesDetailed(name, now, false)
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

// ContextLength 对外模型名声明的上下文窗口（tokens），0 = 未声明。
// 路由组：手动配置优先；未配置时取成员中已配置的最小值（保守，组内任一模型装不下
// 就不虚报）。同名多条模型配置同样取最小值。调用方只在返回值 > 0 时输出该字段。
func (s *Store) ContextLength(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	min := int64(0)
	take := func(v int64) {
		if v > 0 && (min == 0 || v < min) {
			min = v
		}
	}
	for _, g := range s.Groups { // 组名优先：同名时客户端请求的是组
		if g.Name != name {
			continue
		}
		if g.ContextLength > 0 {
			return g.ContextLength
		}
		for _, mem := range g.Members {
			if m := s.modelByIDLocked(mem.ModelID); m != nil {
				take(m.ContextLength)
			}
		}
		return min
	}
	for _, m := range s.Models {
		if m.Name == name {
			take(m.ContextLength)
		}
	}
	return min
}

// ModelNames 去重后的对外模型名列表（仅统计开启了 Expose 的路由）
func (s *Store) ModelNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, m := range s.Models {
		if m.Expose {
			set[m.Name] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Path 状态文件路径（后台展示与测试用）
func (s *Store) Path() string { return s.path }

// Lock/Unlock 仅在需要一次性检查内部状态时使用（测试与迁移期兼容）。
func (s *Store) Lock() { s.mu.Lock() }

// Unlock 与 Lock 配对。
func (s *Store) Unlock() { s.mu.Unlock() }

// Save 强制立即落盘配置与热数据（测试与升级前收尾使用）。
func (s *Store) Save() {
	s.mu.Lock()
	s.save()
	s.mu.Unlock()
	s.Flush()
}

// SkipReasonFor 返回该模型不可用的原因（空串 = 可用），供诊断与测试使用。
func (s *Store) SkipReasonFor(m *Model, now time.Time, today string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipReason(m, now, today)
}
