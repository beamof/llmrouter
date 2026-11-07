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
	Providers []*Provider         `json:"providers"`
	Models    []ModelView         `json:"models"`
	APIKeys   []*APIKey           `json:"api_keys"`
	Usage     []*UsageStat        `json:"usage"`
	Balances  map[string]*Balance `json:"balances"`
	Logs      []*LogEntry         `json:"logs"`
	AutoModel string              `json:"auto_model"`
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
	v := StateView{Providers: []*Provider{}, Models: []ModelView{}, APIKeys: []*APIKey{}, Usage: []*UsageStat{}, Balances: map[string]*Balance{}, Logs: []*LogEntry{}}
	if s.Providers != nil {
		v.Providers = s.Providers
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
	v.AutoModel = s.AutoModel // 已持锁，直接读；空值由前端按默认 auto 处理
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
	if p.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "base_url 不能为空")
		return
	}
	s.store.UpsertProvider(&p)
	s.replyState(w, r)
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteProvider(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
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
	s.store.UpsertModel(&m)
	s.replyState(w, r)
}

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteModel(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "model 不存在")
		return
	}
	s.replyState(w, r)
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.store.Reorder(req.IDs)
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

// handleSettings 更新设置（目前只有 auto 虚拟模型名）
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoModel string `json:"auto_model"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.AutoModel)
	if name == "" {
		name = "auto"
	}
	s.store.SetAutoModel(name)
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
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	start := time.Now()
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
	client := &http.Client{Timeout: 15 * time.Second}
	if resp, err := client.Do(req); err != nil {
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
