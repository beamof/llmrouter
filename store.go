package main

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
)

// defaultGroupID 默认路由组的固定 ID，不占用 NextID（保持旧数据里 p1/m1 等 ID 序列不变）
const defaultGroupID = "default"

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
}

// Provider 一个上游服务商
type Provider struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	BaseURL      string           `json:"base_url"`                // chat completions 基址，填到 /v1，如 https://api.openai.com/v1
	ResponsesURL string           `json:"responses_url,omitempty"` // responses 协议基址，空 = 同 BaseURL（如智谱两协议不同基址时分别填写）
	APIKey       string           `json:"api_key,omitempty"`       // 上游密钥
	BalanceURL   string           `json:"balance_url,omitempty"`   // 可选的余额查询地址（GET + 本密钥）
	Proxy        string           `json:"proxy,omitempty"`         // 可选代理地址，支持 http(s):// 与 socks5(h)://；空 = 不代理（仍读环境变量）
	Kind         string           `json:"kind,omitempty"`          // 上游类型：空 = 标准 OpenAI 兼容；"workbuddy" / "workbuddy-intl" = 复用 WorkBuddy 客户端登录态；"zed" = Zed 账号；"commandcode" = Command Code 订阅（CLI 线协议）
	WB           *WorkBuddyCred   `json:"wb,omitempty"`            // WorkBuddy 凭证（Kind 为 workbuddy 系时使用，凭证明文存于状态文件）
	Zed          *ZedCred         `json:"zed,omitempty"`           // Zed 凭证（Kind 为 zed 时使用，凭证明文存于状态文件）
	CommandCode  *CommandCodeCred `json:"commandcode,omitempty"`   // Command Code 凭证（Kind 为 commandcode 时使用）
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
}

// expires 取 accessToken 过期时刻（nil 安全）
func (c *WorkBuddyCred) expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// refreshExpires 取 refreshToken 过期时刻（nil 安全）
func (c *WorkBuddyCred) refreshExpires() time.Time {
	if c == nil || c.RefreshExpiresAt == nil {
		return time.Time{}
	}
	return *c.RefreshExpiresAt
}

// ZedCred Zed 上游的账号凭证（后台原生登录获得）。NativeCredential 是 zed.dev
// 回调解密出的长期原生凭据（JSON 对象原文，作为整体参与鉴权）；AccessToken 是
// 用它换取的短期推理 JWT，临期由 zedRefreshToken 用原生凭据重铸，无需刷新令牌。
type ZedCred struct {
	UserID           string          `json:"user_id"`
	SystemID         string          `json:"system_id,omitempty"`         // 本机 Zed 安装标识（可选；试用额度绑定真实安装，空 = 不带该头，由上游决定）
	OrganizationID   string          `json:"organization_id,omitempty"`   // 铸 llm_tokens 的组织 UUID（个人账号 = 个人组织，登录时从 users/me 选定）
	NativeCredential json.RawMessage `json:"native_credential,omitempty"` // 长期原生凭据 JSON 原文
	AccessToken      string          `json:"access_token,omitempty"`      // 短期推理 JWT
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"`        // JWT 过期时刻，nil = 未知
	Username         string          `json:"username,omitempty"`
	GitHubLogin      string          `json:"github_login,omitempty"`
	Plan             *ZedPlan        `json:"plan,omitempty"` // 最近一次账号/套餐快照（刷新账号信息时更新）
}

// ZedPlan Zed 账号套餐与模型请求用量快照
type ZedPlan struct {
	PlanType        string    `json:"plan_type,omitempty"`
	Used            *int64    `json:"used,omitempty"`  // 周期内已用模型请求数
	Limit           *int64    `json:"limit,omitempty"` // 周期上限，nil = 不限/未知
	SubscriptionEnd string    `json:"subscription_end,omitempty"`
	CheckedAt       time.Time `json:"checked_at"`
}

// expires 取 JWT 过期时刻（nil 安全）
func (c *ZedCred) expires() time.Time {
	if c == nil || c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

// CommandCodeCred Command Code 订阅（commandcode.ai）的账号凭证。密钥长期有效，
// 无需刷新；Plan 是最近一次读取的套餐名（Go / Pro / GOAT / Max / Ultra 等），
// 用于按套餐过滤模型。CLI 为 true 表示密钥来自本机 `commandcode login` 的
// ~/.commandcode/auth.json，刷新时可重新读取（CLI 换号后自动跟上）。
type CommandCodeCred struct {
	APIKey    string     `json:"api_key"`
	UserID    string     `json:"user_id,omitempty"`
	UserName  string     `json:"user_name,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	CLI       bool       `json:"cli,omitempty"` // 来自本机 CLI 登录态
	ZDR       bool       `json:"zdr,omitempty"` // 请求零数据保留（x-cmd-zdr: 1）
	CheckedAt *time.Time `json:"checked_at,omitempty"`
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
	Name    string `json:"name"` // 模型名：直接调用时的 model 名，也是发往上游的 model 字段
	// Priority 直连优先级：直接指定模型名时的排序值，由各路由组成员优先级的最大值自动维护；
	// 组内选路使用 GroupMember.Priority，不使用该字段。
	Priority      int          `json:"priority"`
	DailyLimit    int64        `json:"daily_limit"`    // 每日请求上限，0 = 不限（全局共享，按模型配置统计）
	MaxConcurrent int          `json:"max_concurrent"` // 最大并行请求数，0 = 不限；全局共享
	Mutes         []MuteWindow `json:"mutes,omitempty"`

	// Expose 该模型名是否出现在 /v1/models 列表里。默认 false：路由组名始终可见，
	// 想让客户端下拉出现真实模型名时再按路由单独打开。同一模型名任一路由开启即暴露；
	// 关闭只影响列表展示，直接指定 model 名或走所在路由组都不受影响。
	Expose bool `json:"expose,omitempty"`

	// MutedUntil 上游返回 429 后自动静音到的时刻（绝对时间），零值 = 未静音。
	// 与 Mutes（每天重复的时段）不同，这是临时的、一次性的，到点自动恢复。
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
// 会话粘性绑定只在内存且会过期，这张表保证过期后的用量与速度仍然可查。
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
}

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
		s.Groups = []*RouteGroup{{ID: defaultGroupID, Name: name, Members: []GroupMember{}}}
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
	keptSession := s.SessionUsage[:0]
	for _, u := range s.SessionUsage {
		if u.Date >= cut {
			keptSession = append(keptSession, u)
		}
	}
	s.SessionUsage = keptSession
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.path+".tmp", b, 0o600); err != nil { // 含上游凭证与 API Key，仅属主可读
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
type Usage struct {
	Prompt     int64 // 输入总 tokens（含缓存命中部分）
	Cached     int64 // 其中命中缓存的 tokens
	Completion int64 // 输出 tokens
}

// AddUsage 记一次调用并落盘（全局按天聚合）
func (s *Store) AddUsage(m *Model, keyID string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addUsageLocked(m, keyID, u)
	s.save()
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
	s.save()
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
			return
		}
	}
	st := &SessionStat{
		Date: today, SessionID: sessionID, KeyID: keyID,
		ProviderID: m.ProviderID, Model: m.Name, Requested: requested,
		Requests: 1, PromptTokens: us.Prompt, CachedTokens: us.Cached, CompletionTokens: us.Completion,
		FirstAt: now, LastAt: now,
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

// Candidate 参与本次路由的一个候选（拷贝，避免和后台编辑竞态）
type Candidate struct {
	Provider Provider
	Model    Model
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
func (s *Store) AddGroup(name, note string) (RouteGroup, error) {
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
	g := &RouteGroup{ID: s.newID("g"), Name: name, Note: strings.TrimSpace(note), Members: []GroupMember{}}
	s.Groups = append(s.Groups, g)
	s.save()
	return cloneGroup(g), nil
}

// UpdateGroup 重命名/改备注；名字同样在组间唯一。
func (s *Store) UpdateGroup(id, name, note string) (RouteGroup, error) {
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

// maxSessionTTL 会话粘性有效期上限，避免超大值在 time.Duration 换算时溢出
const maxSessionTTL = 30 * 24 * time.Hour

// sessionTTLLocked 当前会话 TTL（调用方需持锁）
func (s *Store) sessionTTLLocked() time.Duration {
	switch {
	case s.SessionTTLSeconds <= 0:
		return sessionTTL
	case s.SessionTTLSeconds > int64(maxSessionTTL/time.Second):
		return maxSessionTTL
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
	case seconds > int64(maxSessionTTL/time.Second):
		seconds = int64(maxSessionTTL / time.Second)
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
		if mutedUntil(m, time.Now()) {
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

// SkipReason 某条模型路由被排除在候选之外的原因（用于会话日志说明选择理由）
type SkipReason struct {
	ModelID    string `json:"model_id"`
	Model      string `json:"model"`
	ProviderID string `json:"provider_id"`
	Reason     string `json:"reason"`
}

// skipReason 该条目为何不可用，返回空串表示可用（调用方需持锁）
func (s *Store) skipReason(m *Model, now time.Time, today string) string {
	switch {
	case mutedUntil(m, now):
		return "上游 429 自动静音至 " + m.MutedUntil.Local().Format("01-02 15:04")
	case mutedNow(m.Mutes, now):
		return "处于静音时段"
	case s.overLimit(m, today):
		return "已达每日 token 上限"
	case s.atCapacity(m):
		return fmt.Sprintf("并行已满 %d/%d", s.inflight[m.ID], m.MaxConcurrent)
	}
	return ""
}

// mutedUntil 该条目是否因上游 429 处于自动静音期
func mutedUntil(m *Model, now time.Time) bool {
	return !m.MutedUntil.IsZero() && now.Before(m.MutedUntil)
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
