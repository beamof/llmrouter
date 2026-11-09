package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ModelView 模型 + 今日已用次数（给后台页面）
type ModelView struct {
	*Model
	UsedToday int64 `json:"used_today"` // 今日已用 tokens
	Inflight  int   `json:"inflight"`   // 当前在途请求数（并行）
}

// StateView 后台页面所需的完整状态
type StateView struct {
	Providers []*Provider   `json:"providers"`
	Groups    []*RouteGroup `json:"groups"`
	Models    []ModelView   `json:"models"`
	APIKeys   []*APIKey     `json:"api_keys"`
	Usage     []*UsageStat  `json:"usage"`
	// SessionUsage 按天 × 会话 聚合的用量，含已过期会话；用量页「按会话」视图用。
	SessionUsage []*SessionStat      `json:"session_usage"`
	Balances     map[string]*Balance `json:"balances"`
	Logs         []*LogEntry         `json:"logs"`
	AutoModel    string              `json:"auto_model"`
	// ResponsesConvert responses 协议转换开关（默认关闭），设置页据此渲染
	ResponsesConvert bool `json:"responses_convert"`
	// SessionTTLSeconds 生效中的会话粘性有效期（秒），设置页据此渲染
	SessionTTLSeconds int64 `json:"session_ttl_seconds"`
}

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
		v.Models = append(v.Models, ModelView{Model: m, UsedToday: used[m.ProviderID+"\x00"+m.Name], Inflight: s.inflight[m.ID]})
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

func (s *Server) replyState(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.store.SnapshotJSON())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleProviderUpsert(w http.ResponseWriter, r *http.Request) {
	var p Provider
	if !decode(w, r, &p) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		p.ID = id
	}
	switch p.Kind {
	case "":
	case "workbuddy", "workbuddy-intl":
		// WorkBuddy 类型：基址固定由地区端点派生，不接受自定义——凭证随请求发出，
		// 允许改写基址等于允许把 accessToken 发往任意主机
		e, _ := wbOf(p)
		if e == nil {
			writeErr(w, http.StatusBadRequest, "kind 非法")
			return
		}
		p.BaseURL = e.endpoint + "/v2"
		if p.WB == nil || strings.TrimSpace(p.WB.AccessToken) == "" {
			writeErr(w, http.StatusBadRequest, "WorkBuddy 类型的 provider 必须携带凭证（wb.access_token），编辑时请勿丢弃 wb 字段")
			return
		}
	case "zed":
		// Zed 类型：数据面基址固定官方端点，不接受自定义（理由同 WorkBuddy：
		// JWT 凭证随请求发出，改写基址等于允许把凭证发往任意主机）
		if p.Zed == nil || strings.TrimSpace(p.Zed.UserID) == "" {
			writeErr(w, http.StatusBadRequest, "Zed 类型的 provider 必须携带凭证（zed.user_id），编辑时请勿丢弃 zed 字段")
			return
		}
		p.BaseURL = s.zedCloud() + "/completions"
	default:
		writeErr(w, http.StatusBadRequest, "kind 只能为空、workbuddy、workbuddy-intl 或 zed")
		return
	}
	if p.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "base_url 不能为空")
		return
	}
	proxyURL, err := normalizeProxy(p.Proxy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "proxy 非法: "+err.Error())
		return
	}
	p.Proxy = proxyURL // 存规范化后的地址（无 scheme 时补 http://）
	s.store.UpsertProvider(&p)
	s.replyState(w, r)
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.DeleteProvider(id) {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	s.wbForgetProvider(id)
	s.zedForgetProvider(id)
	s.replyState(w, r)
}

func (s *Server) handleModelUpsert(w http.ResponseWriter, r *http.Request) {
	var m Model
	if !decode(w, r, &m) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		m.ID = id
	}
	if m.Name == "" || m.ProviderID == "" {
		writeErr(w, http.StatusBadRequest, "name 与 provider_id 不能为空")
		return
	}
	if _, ok := s.store.Provider(m.ProviderID); !ok {
		writeErr(w, http.StatusBadRequest, "provider 不存在")
		return
	}
	// group_id 只是「在哪个组里编辑/创建」的上下文；模型配置本身全局共享，
	// priority 写入该组的成员优先级，不影响模型在其他组的优先级。
	groupID := m.GroupID
	if err := s.store.UpsertModelInGroup(&m, groupID, m.Priority); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.replyState(w, r)
}

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteModel(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "model 不存在")
		return
	}
	s.replyState(w, r)
}

// handleGroupAttachModel 把已有的共享模型配置加入某路由组，并设置组内优先级
func (s *Server) handleGroupAttachModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelID  string `json:"model_id"`
		Priority int    `json:"priority"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.ModelID == "" {
		writeErr(w, http.StatusBadRequest, "model_id 不能为空")
		return
	}
	if err := s.store.AttachModelToGroup(r.PathValue("id"), req.ModelID, req.Priority); err != nil {
		status := http.StatusBadRequest
		if _, ok := s.store.GroupByID(r.PathValue("id")); !ok {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	s.replyState(w, r)
}

// handleGroupDetachModel 把模型移出路由组；若它已不属于任何组，共享配置一并删除
func (s *Server) handleGroupDetachModel(w http.ResponseWriter, r *http.Request) {
	if !s.store.DetachModelFromGroup(r.PathValue("id"), r.PathValue("modelID")) {
		writeErr(w, http.StatusNotFound, "该路由组中不存在这个模型")
		return
	}
	s.replyState(w, r)
}

// handleGroupCreate 新建路由组：组名即对外虚拟模型名，请求该名字时只在组内选路
func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.store.AddGroup(req.Name, req.Note); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.replyState(w, r)
}

// handleGroupUpdate 重命名路由组 / 修改备注
func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.store.UpdateGroup(r.PathValue("id"), req.Name, req.Note); err != nil {
		status := http.StatusBadRequest
		if _, ok := s.store.GroupByID(r.PathValue("id")); !ok {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	s.replyState(w, r)
}

// handleGroupDelete 删除路由组及其下全部模型；至少保留一个组
func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.GroupByID(id); !ok {
		writeErr(w, http.StatusNotFound, "路由组不存在")
		return
	}
	if !s.store.DeleteGroup(id) {
		writeErr(w, http.StatusBadRequest, "至少保留一个路由组")
		return
	}
	s.replyState(w, r)
}

// handleModelExpose 切换该模型名是否出现在 /v1/models 列表里（不影响指定 model 名直接调用）
func (s *Server) handleModelExpose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expose bool `json:"expose"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !s.store.SetModelExpose(r.PathValue("id"), req.Expose) {
		writeErr(w, http.StatusNotFound, "model 不存在")
		return
	}
	s.replyState(w, r)
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs     []string `json:"ids"`
		GroupID string   `json:"group_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.GroupID == "" {
		req.GroupID = s.store.DefaultGroupID()
	}
	if !s.store.ReorderGroup(req.GroupID, req.IDs) {
		writeErr(w, http.StatusNotFound, "路由组不存在")
		return
	}
	s.replyState(w, r)
}

// handleLogsClear 清空异常日志
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.store.ClearLogs()
	s.replyState(w, r)
}

// handleModelUnmute 手动解除上游 429 触发的自动静音
func (s *Server) handleModelUnmute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if m, ok := s.store.Model(id); ok && m.MutedUntil.IsZero() {
		writeErr(w, http.StatusBadRequest, "该模型当前没有自动静音")
		return
	}
	if !s.store.ClearMute(id) {
		writeErr(w, http.StatusNotFound, "模型不存在")
		return
	}
	s.replyState(w, r)
}

// handleSettings 更新设置：默认路由组名、responses 协议转换开关、会话 TTL。
// 字段用指针，未提交的项保持原值（前端可以只改其中一项）。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoModel         *string `json:"auto_model"`
		ResponsesConvert  *bool   `json:"responses_convert"`
		SessionTTLSeconds *int64  `json:"session_ttl_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.AutoModel != nil {
		name := strings.TrimSpace(*req.AutoModel)
		if name == "" {
			name = "auto"
		}
		if err := s.store.SetAutoModel(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.ResponsesConvert != nil {
		s.store.SetResponsesConvert(*req.ResponsesConvert)
	}
	if req.SessionTTLSeconds != nil {
		s.store.SetSessionTTL(*req.SessionTTLSeconds)
		s.applySessionTTL(s.store.SessionTTL()) // 现有绑定也按新 TTL 立即重算
	}
	s.replyState(w, r)
}

// handleSessionEnd 主动结束会话：删除该 X-Session-ID 下所有协议/模型绑定。
// 会话 ID 走查询参数而非路径段，因为客户端可能使用含斜杠等字符的标识。
func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id 参数")
		return
	}
	if n := s.endSession(id); n == 0 {
		writeErr(w, http.StatusNotFound, "会话不存在或已过期")
		return
	}
	s.replyState(w, r)
}

// handleKeyCreate 生成一个客户端 API Key
func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = "key"
	}
	writeJSON(w, http.StatusOK, s.store.AddAPIKey(req.Name))
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteAPIKey(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	s.replyState(w, r)
}

// handleUpstreamModels 拉取上游 /models（OpenAI 格式），供添加模型时下拉选择
func (s *Server) handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Provider(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	// WorkBuddy：模型清单来自 /v3/config 目录（含缓存与内置兜底）
	if e, cred := wbOf(p); e != nil {
		if cred == nil || cred.AccessToken == "" {
			writeErr(w, http.StatusBadRequest, "该 provider 未导入 WorkBuddy 凭证")
			return
		}
		ids := append([]string(nil), s.wbCatalogFor(p).IDs...)
		sort.Strings(ids)
		writeJSON(w, http.StatusOK, map[string]any{"models": ids})
		return
	}
	// Zed：目录在 cloud.zed.dev/models（JWT + 原生客户端头），BaseURL 是
	// completions 端点，不能直接拼 /models
	if zedOf(p) != nil {
		models, err := s.zedCatalog(p)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "拉取 Zed 模型目录失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
		return
	}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	cli, cerr := s.upstreamClient(p, 15*time.Second)
	if cerr != nil {
		writeErr(w, http.StatusBadRequest, cerr.Error())
		return
	}
	resp, err := cli.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取上游模型列表失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := string(b)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("上游返回 HTTP %d: %s", resp.StatusCode, msg))
		return
	}
	ids := extractModelIDs(b)
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

// extractModelIDs 从 /models 响应中提取模型名，兼容三种格式：
// OpenAI 的 {"data":[{"id":...}]}、GLM/Anthropic 风格的 {"models":[{"slug":...}]}、纯字符串数组
func extractModelIDs(b []byte) []string {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range v.Data {
		add(m.ID)
	}
	for _, raw := range v.Models {
		var m struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &m) == nil {
			switch {
			case m.ID != "":
				add(m.ID)
				continue
			case m.Slug != "":
				add(m.Slug)
				continue
			case m.Name != "":
				add(m.Name)
				continue
			}
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			add(s)
		}
	}
	return ids
}

// handleModelTest 探测该模型的上游是否可达：GET {base}/models，不发对话、不耗 token。
// 顺带用返回的列表核对模型名、用状态码核对鉴权。不走路由过滤（静音/限流），也不计入用量。
func (s *Server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.Model(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "model 不存在")
		return
	}
	p, ok := s.store.Provider(m.ProviderID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "provider 不存在")
		return
	}
	// WorkBuddy：用 /v3/config 目录做连通性探测，并核对模型名
	if e, cred := wbOf(p); e != nil {
		if cred == nil || cred.AccessToken == "" {
			writeJSON(w, http.StatusOK, map[string]any{"level": "warn", "message": "未导入 WorkBuddy 凭证，请先在 Provider 弹窗导入"})
			return
		}
		s.wbModelTest(w, m, p, e, cred)
		return
	}
	// Zed：目录在 cloud.zed.dev/models（JWT + 原生客户端头），核对模型名在目录中
	if zedOf(p) != nil {
		start := time.Now()
		models, err := s.zedCatalog(p)
		res := map[string]any{"latency_ms": time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			res["level"] = "fail"
			res["message"] = "连接失败: " + err.Error()
		default:
			res["level"] = "ok"
			res["message"] = fmt.Sprintf("端口通，模型名在 Zed 目录中（共 %d 个）", len(models))
			found := false
			for _, id := range models {
				if id == m.Name {
					found = true
					break
				}
			}
			if !found {
				res["level"] = "warn"
				res["message"] = fmt.Sprintf("端口通，但 Zed 目录（%d 个）中没有 %q，请核对模型名", len(models), m.Name)
			}
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	start := time.Now()
	cli, cerr := s.upstreamClient(p, 15*time.Second)
	if cerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"level": "fail", "message": cerr.Error()})
		return
	}
	resp, err := cli.Do(req)
	res := map[string]any{"latency_ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["level"] = "fail"
		res["message"] = "连接失败: " + err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res["status"] = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res["level"] = "warn"
		res["message"] = fmt.Sprintf("端口通（HTTP %d，鉴权失败，请检查 API Key）: %s", resp.StatusCode, errSnippet(b))
	case resp.StatusCode >= 400:
		res["level"] = "warn"
		res["message"] = fmt.Sprintf("端口通，但 /models 返回 HTTP %d（不影响对话接口）", resp.StatusCode)
	default:
		res["level"] = "ok"
		res["message"] = "端口通"
		if ids := extractModelIDs(b); len(ids) > 0 {
			found := false
			for _, id := range ids {
				if id == m.Name {
					found = true
					break
				}
			}
			if found {
				res["message"] = fmt.Sprintf("端口通，模型名在上游列表中（共 %d 个）", len(ids))
			} else {
				res["level"] = "warn"
				res["message"] = fmt.Sprintf("端口通，但上游列表（%d 个）中没有 %q，请核对模型名", len(ids), m.Name)
			}
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// errSnippet 提取上游错误信息，没有则截断原始响应
func errSnippet(b []byte) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil && v.Error != nil && v.Error.Message != "" {
		return v.Error.Message
	}
	s := string(b)
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80]) + "…"
	}
	return s
}

var balanceKeyRe = regexp.MustCompile(`(?i)balance|credit|quota|amount|remaining|money`)

// 常见厂商的官方余额接口，基址域名命中且未单独配置时自动使用
var defaultBalanceURLs = map[string]string{
	"open.bigmodel.cn": "https://open.bigmodel.cn/api/monitor/usage/quota/limit",
	"api.deepseek.com": "https://api.deepseek.com/user/balance",
}

// balanceEndpoint 余额查询地址：优先 provider 配置，否则按基址域名给内置默认
func balanceEndpoint(p Provider) string {
	if p.BalanceURL != "" {
		return p.BalanceURL
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return ""
	}
	return defaultBalanceURLs[u.Hostname()]
}

// handleBalance 拉取上游余额：GET 余额接口，带上该 provider 的密钥
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.store.Provider(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	ep := balanceEndpoint(p)
	if ep == "" {
		writeErr(w, http.StatusBadRequest, "该 provider 未配置余额查询 URL，且其域名没有内置默认接口")
		return
	}
	req, _ := http.NewRequest(http.MethodGet, ep, nil)
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	b := &Balance{At: time.Now()}
	cli, cerr := s.upstreamClient(p, 15*time.Second)
	if cerr != nil {
		b.Raw = cerr.Error()
	} else if resp, err := cli.Do(req); err != nil {
		b.Raw = err.Error()
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		b.OK = resp.StatusCode == http.StatusOK
		b.Raw = string(body)
		if b.OK {
			if s := zhipuQuotaSummary(body); len(s) > 0 {
				b.Found = s
			} else {
				b.Found = findBalances(body)
			}
		}
	}
	s.store.SetBalance(id, b)
	writeJSON(w, http.StatusOK, b)
}

// zhipuQuotaSummary 把智谱 Coding Plan 的套餐额度响应转成友好文案；
// 不是该格式时返回 nil，走通用余额字段提取
func zhipuQuotaSummary(body []byte) []string {
	var v struct {
		Data *struct {
			Level  string `json:"level"`
			Limits []struct {
				Type         string `json:"type"`
				Usage        int    `json:"usage"`
				CurrentValue int    `json:"currentValue"`
				Remaining    int    `json:"remaining"`
				Percentage   int    `json:"percentage"`
				NextReset    int64  `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Data == nil || len(v.Data.Limits) == 0 {
		return nil
	}
	reset := func(ms int64) string {
		if ms <= 0 {
			return ""
		}
		return "，" + time.UnixMilli(ms).Format("01-02 15:04") + " 重置"
	}
	var out []string
	for _, l := range v.Data.Limits {
		switch l.Type {
		case "TOKENS_LIMIT":
			out = append(out, fmt.Sprintf("Token 额度已用 %d%%%s", l.Percentage, reset(l.NextReset)))
		case "TIME_LIMIT":
			out = append(out, fmt.Sprintf("MCP 次数 %d/%d（剩 %d）%s", l.CurrentValue, l.Usage, l.Remaining, reset(l.NextReset)))
		}
	}
	if v.Data.Level != "" {
		out = append(out, "套餐等级 "+v.Data.Level)
	}
	return out
}

// findBalances 在任意 JSON 里挑出长得像余额的数值字段（如 totalBalance = 110.25）。
// 只认可解析为数字的值（避免把 balanceType 这类字符串当成余额），
// 键名含 total/remaining/avail 的排前面。
func findBalances(body []byte) []string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	type hit struct {
		path, key string
		val       float64
	}
	var out []hit
	var walk func(path string, x any)
	walk = func(path string, x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				p := path + "." + k
				if balanceKeyRe.MatchString(k) && isLeaf(val) {
					if f, ok := numericVal(val); ok {
						out = append(out, hit{strings.TrimPrefix(p, "."), strings.ToLower(k), f})
					}
				}
				walk(p, val)
			}
		case []any:
			for i, val := range t {
				walk(fmt.Sprintf("%s.%d", path, i), val)
			}
		}
	}
	walk("", v)
	sort.SliceStable(out, func(i, j int) bool {
		return balancePriority(out[i].key) < balancePriority(out[j].key)
	})
	res := make([]string, 0, len(out))
	for _, h := range out {
		res = append(res, fmt.Sprintf("%s = %v", h.path, h.val))
	}
	if len(res) > 8 {
		res = res[:8]
	}
	return res
}

// numericVal 只把数字或数字字符串当作余额值
func numericVal(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(x), ",", ""), 64)
		return f, err == nil
	}
	return 0, false
}

func balancePriority(lowerKey string) int {
	if strings.Contains(lowerKey, "total") || strings.Contains(lowerKey, "remaining") || strings.Contains(lowerKey, "avail") {
		return 0
	}
	return 1
}

func isLeaf(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// ─────────────────── WorkBuddy 凭证扫描 / 导入 / 续期 ───────────────────

// handleWBScan 扫描本机 WorkBuddy/CodeBuddy 客户端的登录态快照。
// 客户端 v5.6.2+ 的快照是加密信封，需在请求体里带 atRestSecretKey 才能解开。
func (s *Server) handleWBScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": wbScan(strings.TrimSpace(req.Key))})
}

// handleWBImport 导入 WorkBuddy 凭证（扫描结果回传 cred，或粘贴 raw JSON）。
// 同一账号（Kind + uid）重复导入时原地更新，不会产生重复 Provider。
func (s *Server) handleWBImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string         `json:"kind"` // workbuddy | workbuddy-intl
		Name string         `json:"name"`
		Cred *WorkBuddyCred `json:"cred"` // 扫描结果里的凭证对象
		Raw  string         `json:"raw"`  // 或粘贴的 .info 形态 JSON
		Key  string         `json:"key"`  // raw 含加密信封时的 atRestSecretKey
	}
	if !decode(w, r, &req) {
		return
	}
	e := &wbCN
	switch req.Kind {
	case "workbuddy":
	case "workbuddy-intl":
		e = &wbIntl
	default:
		writeErr(w, http.StatusBadRequest, "kind 必须是 workbuddy 或 workbuddy-intl")
		return
	}
	cred := req.Cred
	if cred == nil && strings.TrimSpace(req.Raw) != "" {
		hit := wbParseInfoBytes([]byte(req.Raw), strings.TrimSpace(req.Key))
		if hit == nil {
			writeErr(w, http.StatusBadRequest, "无法从粘贴的 JSON 解析出凭证（需要 uid 与 accessToken 字段）")
			return
		}
		if hit.Encrypted || hit.Cred == nil {
			writeErr(w, http.StatusBadRequest, "粘贴的凭证是加密信封，请提供正确的 atRestSecretKey")
			return
		}
		cred = hit.Cred
	}
	if cred == nil || strings.TrimSpace(cred.AccessToken) == "" {
		writeErr(w, http.StatusBadRequest, "缺少凭证（cred 或 raw 二选一）")
		return
	}
	if strings.TrimSpace(cred.UID) == "" {
		writeErr(w, http.StatusBadRequest, "凭证缺少 uid 字段")
		return
	}
	p := &Provider{Kind: req.Kind, BaseURL: e.endpoint + "/v2", WB: cred}
	if old, ok := s.store.ProviderByWBUID(req.Kind, cred.UID); ok {
		p.ID, p.Name, p.Proxy = old.ID, old.Name, old.Proxy // 原地更新（基址始终由地区端点派生，代理配置沿用）
	} else {
		p.Name = strings.TrimSpace(req.Name)
		if p.Name == "" {
			uid := cred.UID
			if len(uid) > 8 {
				uid = uid[:8]
			}
			p.Name = "WorkBuddy " + e.label + " · " + uid
		}
	}
	s.store.UpsertProvider(p)
	s.wbInvalidateCatalog(p.ID)
	s.replyState(w, r)
}

// handleWBRefresh 手动触发续期并拉取远端模型目录
func (s *Server) handleWBRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Provider(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	if e, _ := wbOf(p); e == nil {
		writeErr(w, http.StatusBadRequest, "该 provider 不是 WorkBuddy 类型")
		return
	}
	cred, err := s.wbRefreshToken(p, true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "续期失败: "+err.Error())
		return
	}
	p.WB = cred // 续期后的凭证在 store 里已换新，这里同步快照，否则拉目录仍用旧 token
	s.wbInvalidateCatalog(p.ID)
	cat := s.wbCatalogFor(p)
	writeJSON(w, http.StatusOK, map[string]any{
		"expires_at": cred.expires(),
		"models":     len(cat.IDs),
		"remote":     cat.Remote,
	})
}

// handleWBCheckin 手动签到（仅国内版；企业账号不做个人签到，改为刷新企业积分），
// 无论哪条路径都顺带刷新一次余额快照。
func (s *Server) handleWBCheckin(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Provider(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	e, cred := wbOf(p)
	if e == nil || cred == nil {
		writeErr(w, http.StatusBadRequest, "该 provider 不是 WorkBuddy 类型")
		return
	}
	if e.intl {
		writeErr(w, http.StatusBadRequest, "国际版不支持签到")
		return
	}
	res := map[string]any{}
	if cred.EnterpriseID != "" {
		res["message"] = "企业账号不支持个人签到，已刷新企业积分"
	} else {
		already, err := s.wbCheckin(p)
		if err != nil {
			s.logFail(LogEntry{Level: "warn", Kind: kindCheckin, ProviderID: p.ID,
				Message: "WorkBuddy 手动签到失败", Detail: err.Error()})
			writeErr(w, http.StatusBadGateway, "签到失败: "+err.Error())
			return
		}
		res["already_checked_in"] = already
		if already {
			res["message"] = "今天已签到，请明天再来"
		} else {
			res["message"] = "签到成功"
		}
	}
	if u, err := s.wbUserResource(p); err == nil {
		res["usage"] = u
	} else {
		res["usage_error"] = err.Error()
	}
	s.logFail(LogEntry{Level: "info", Kind: kindCheckin, ProviderID: p.ID, Message: "WorkBuddy 手动签到：" + res["message"].(string)})
	writeJSON(w, http.StatusOK, res)
}

// handleWBUsage 手动刷新 WorkBuddy 余额快照（个人资源包 / 企业积分）
func (s *Server) handleWBUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Provider(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	if e, _ := wbOf(p); e == nil {
		writeErr(w, http.StatusBadRequest, "该 provider 不是 WorkBuddy 类型")
		return
	}
	u, err := s.wbUserResource(p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "余额查询失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": u})
}

// handleSessions 当前活跃的粘性会话列表（带 X-Session-ID 的客户端绑定；仅内存，重启即清）
func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.activeSessions()})
}

// wbModelTest WorkBuddy 模型探测：GET /v3/config（daemon UA + 凭证），
// 核对模型名是否在目录中，顺带把目录写进缓存
func (s *Server) wbModelTest(w http.ResponseWriter, m Model, p Provider, e *wbEdition, cred *WorkBuddyCred) {
	start := time.Now()
	cli, cerr := s.upstreamClient(p, 20*time.Second)
	if cerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"level": "fail", "message": cerr.Error()})
		return
	}
	cat, err := wbFetchCatalog(cli, wbEndpoint(p), e, cred)
	res := map[string]any{"latency_ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["level"] = "warn"
		res["message"] = "目录接口异常（不影响对话接口）: " + err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	if len(cat.IDs) > 0 { // 空目录不入缓存，否则会盖掉可用的兜底清单
		s.wbMu.Lock()
		if s.wbCats == nil {
			s.wbCats = map[string]*wbCatalog{}
		}
		s.wbCats[p.ID] = cat
		s.wbMu.Unlock()
	}
	if len(cat.IDs) == 0 {
		res["level"] = "warn"
		res["message"] = "端口通，但目录为空（可能无权访问模型清单）"
		writeJSON(w, http.StatusOK, res)
		return
	}
	res["level"] = "ok"
	res["message"] = fmt.Sprintf("端口通，目录共 %d 个模型", len(cat.IDs))
	for _, id := range cat.IDs {
		if strings.EqualFold(id, m.Name) {
			writeJSON(w, http.StatusOK, res)
			return
		}
	}
	res["level"] = "warn"
	res["message"] = fmt.Sprintf("端口通，但目录（%d 个）中没有 %q，请核对模型名", len(cat.IDs), m.Name)
	writeJSON(w, http.StatusOK, res)
}
