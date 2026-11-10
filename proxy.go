package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// hop 逐跳头，不参与透传
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// skipNoResponses 候选无法承接 responses 请求时的剔除原因（用于会话日志）
const skipNoResponses = "不支持 responses 协议（上游仅提供 chat completions）"

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// 会话粘性：客户端带 X-Session-ID 头时，同一会话绑定同一上游，
// 仅当绑定上游失败（fallback）才按规则重新选择并重新绑定。仅存内存，重启即清。
type session struct {
	modelID   string    // 绑定的模型路由 ID
	keyID     string    // 命中的客户端 Key（"env" = 环境变量令牌，空 = 免鉴权）
	requested string    // 客户端请求的 model 名（命中路由组时与绑定模型名不同）
	firstAt   time.Time // 首次绑定时刻
	lastAt    time.Time // 最近一次成功请求时刻
	count     int       // 会话内成功请求数
	expires   time.Time

	// 吞吐统计：只累计上游报告了 usage 的成功请求。平均速度用「累计输出 token /
	// 累计生成耗时」，避免一次慢请求把均值带偏。
	outTokens     int64         // 累计输出 tokens
	genNanos      int64         // 累计生成耗时（流式 = 首字节→结束；非流式 = 端到端）
	lastTokPerSec float64       // 最近一次请求的输出速度
	ttftNanos     int64         // 累计首字节耗时（仅流式）
	ttftSamples   int           // 有首字节计时的请求数（仅流式）
	lastTTFT      time.Duration // 最近一次流式请求的首字节耗时

	// 思考等级：最近一次成功请求的客户端声明值与实际出站值（网关可能注入默认档
	// 或按模型能力收敛，"off" 表示关闭思考，空 = 未声明/未下发）
	effortDeclared string
	effortSent     string
}

const sessionTTL = time.Hour

func (s *Server) getSession(key string) (string, bool) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[key]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, key)
		return "", false
	}
	return sess.modelID, true
}

func (s *Server) setSession(key, modelID, keyID, requested string) {
	ttl := sessionTTL
	if s.store != nil { // 单元测试可能只构造 Server 的会话表
		ttl = s.store.SessionTTL()
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	if len(s.sessions) > 1024 { // 顺手清理过期会话
		now := time.Now()
		for k, v := range s.sessions {
			if now.After(v.expires) {
				delete(s.sessions, k)
			}
		}
	}
	now := time.Now()
	old := s.sessions[key]
	sess := session{
		modelID: modelID, keyID: keyID, requested: requested,
		firstAt: old.firstAt, count: old.count + 1,
		lastAt: now, expires: now.Add(ttl),
		outTokens: old.outTokens, genNanos: old.genNanos, lastTokPerSec: old.lastTokPerSec,
		ttftNanos: old.ttftNanos, ttftSamples: old.ttftSamples, lastTTFT: old.lastTTFT,
		effortDeclared: old.effortDeclared, effortSent: old.effortSent,
	}
	if sess.firstAt.IsZero() {
		sess.firstAt = now
	}
	s.sessions[key] = sess
}

func (s *Server) clearSession(key string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	delete(s.sessions, key)
}

// endSession 主动结束某个会话 ID 下的全部绑定（同一会话在不同协议/请求模型名下
// 可能各有一条）。返回实际删除的绑定数。
func (s *Server) endSession(id string) int {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	prefix := id + "\x00"
	n := 0
	for k := range s.sessions {
		if k == id || strings.HasPrefix(k, prefix) {
			delete(s.sessions, k)
			n++
		}
	}
	return n
}

// applySessionTTL 把新 TTL 立即应用到现有绑定：按最近活跃时刻重算到期时间，
// 已超过新 TTL 的条目顺手删除。
func (s *Server) applySessionTTL(ttl time.Duration) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		exp := v.lastAt.Add(ttl)
		if now.After(exp) {
			delete(s.sessions, k)
			continue
		}
		v.expires = exp
		s.sessions[k] = v
	}
}

// sessionInfo 活跃会话的对外展示形状（GET /api/sessions）
type sessionInfo struct {
	ID        string    `json:"id"`
	KeyID     string    `json:"key_id,omitempty"`
	ModelID   string    `json:"model_id"`
	Requested string    `json:"requested,omitempty"`
	Count     int       `json:"count"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	Expires   time.Time `json:"expires"`

	// 吞吐：输出 tokens 取上游 usage 报告的 completion/output 分项；速度取累计值，
	// 样本不足时为 0。
	OutTokens     int64   `json:"out_tokens,omitempty"`
	TokPerSec     float64 `json:"tok_per_sec,omitempty"`      // 平均输出速度（tokens/s）
	LastTokPerSec float64 `json:"last_tok_per_sec,omitempty"` // 最近一次请求的输出速度
	TTFTMs        int64   `json:"ttft_ms,omitempty"`          // 平均首字节耗时（毫秒，仅流式）
	LastTTFTMs    int64   `json:"last_ttft_ms,omitempty"`     // 最近一次流式请求的首字节耗时

	// 思考等级：客户端声明值（可能为空）与实际出站值（含网关默认注入/能力收敛）
	EffortDeclared string `json:"effort_declared,omitempty"`
	EffortSent     string `json:"effort_sent,omitempty"`
}

// activeSessions 当前活跃（未过期）的会话绑定，按最近活跃倒序
func (s *Server) activeSessions() []sessionInfo {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	now := time.Now()
	out := make([]sessionInfo, 0, len(s.sessions))
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
			continue
		}
		id := k // 粘性键 = 会话ID\x00请求模型；展示用纯会话 ID（模型名单独在 requested）
		if i := strings.IndexByte(k, 0x00); i >= 0 {
			id = k[:i]
		}
		info := sessionInfo{
			ID: id, KeyID: v.keyID, ModelID: v.modelID, Requested: v.requested,
			Count: v.count, FirstAt: v.firstAt, LastAt: v.lastAt, Expires: v.expires,
			OutTokens: v.outTokens, LastTokPerSec: v.lastTokPerSec,
			LastTTFTMs:     int64(v.lastTTFT / time.Millisecond),
			EffortDeclared: v.effortDeclared, EffortSent: v.effortSent,
		}
		if v.genNanos > 0 {
			info.TokPerSec = float64(v.outTokens) / (float64(v.genNanos) / float64(time.Second))
		}
		if v.ttftSamples > 0 {
			info.TTFTMs = int64(time.Duration(v.ttftNanos/int64(v.ttftSamples)) / time.Millisecond)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}

// addSessionUsage 把一次成功请求的用量与计时并入会话统计。会话可能在这期间过期
// 被清理（很慢的请求），此时直接忽略，不影响下次重新建立绑定。
func (s *Server) addSessionUsage(key string, sm usageSample) {
	if key == "" {
		return
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[key]
	if !ok || time.Now().After(sess.expires) {
		return
	}
	if sm.Completion > 0 {
		sess.outTokens += sm.Completion
		sess.genNanos += int64(sm.Gen)
		sess.lastTokPerSec = sm.tokPerSec()
	}
	if sm.Stream && sm.TTFT > 0 {
		sess.ttftNanos += int64(sm.TTFT)
		sess.ttftSamples++
		sess.lastTTFT = sm.TTFT
	}
	// 思考等级按「最近一次请求」写：同会话中途改档位时列表显示最新的值
	sess.effortDeclared = sm.EffortDeclared
	sess.effortSent = sm.EffortSent
	s.sessions[key] = sess
}

// effortArg 客户端显式给的思考等级：reasoning_effort / reasoningEffort /
// reasoning.effort 三种写法都认（Responses 协议用 reasoning.effort）。
func effortArg(body map[string]any) (string, bool) {
	for _, v := range []any{body["reasoning_effort"], body["reasoningEffort"]} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), true
		}
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if s, ok := r["effort"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), true
		}
	}
	return "", false
}

// requestEffort 客户端声明的思考等级；thinking.type=disabled 记为 "off"。
// 把请求体解成 map 只是为了让别名/嵌套写法走同一套解析。
func requestEffort(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return ""
	}
	if t, _ := m["thinking"].(map[string]any); t != nil {
		if s, _ := t["type"].(string); strings.EqualFold(strings.TrimSpace(s), "disabled") {
			return "off"
		}
	}
	if s, ok := effortArg(m); ok {
		return s
	}
	return ""
}

// handleProxy 生成 /chat/completions 与 /responses 共用的透传 handler。
// 请求体只读取 model 字段用于路由，其余字节原样转发；响应字节级回传。
func (s *Server) handleProxy(suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID, ok := s.checkAPIToken(r)
		if !ok {
			s.logFail(LogEntry{
				Level: "warn", Kind: kindAuth, Status: http.StatusUnauthorized,
				Message: "客户端 API Key 无效，请求被拒绝",
			})
			writeErr(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.logFail(LogEntry{
				Level: "error", Kind: kindBadRequest, Status: http.StatusBadRequest, KeyID: keyID,
				Message: "读取请求体失败: " + err.Error(),
			})
			writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
			s.logFail(LogEntry{
				Level: "warn", Kind: kindBadRequest, Status: http.StatusBadRequest, KeyID: keyID,
				Message: `请求体不是合法 JSON 或缺少 "model" 字段`,
				Detail:  truncateRunes(string(body), 200),
			})
			writeErr(w, http.StatusBadRequest, `请求体必须是 JSON 且包含 "model" 字段`)
			return
		}

		// 路由组：组名即对外虚拟模型名，命中组时只在组内按优先级挑选实际模型
		group, isGroup := s.store.GroupByName(req.Model)
		effort := requestEffort(body) // 客户端声明的思考等级，随会话记录（实际出站值由适配层覆盖）
		protocol := "chat"
		if suffix == "/responses" {
			protocol = "responses"
		}
		// responses 协议转换开关（默认关闭）：关闭时仅支持 chat 的上游（WorkBuddy）
		// 不承接 /responses，路由会改选原生支持 responses 的模型。
		responsesConvert := s.store.ResponsesConvertEnabled()

		// 会话绑定的候选排最前（不套用静音/上限规则），规则候选作为 fallback 池。
		// 绑定键含接入协议：同一会话混用两种协议时各自绑定，避免因协议能力不同来回换模型。
		sessKey := ""
		sid := r.Header.Get("X-Session-ID")
		if sid != "" {
			sessKey = sid + "\x00" + protocol + "\x00" + req.Model
		}
		now := time.Now()
		var cands []Candidate
		seen := map[string]bool{}
		addCand := func(c Candidate) {
			if seen[c.Model.ID] {
				return
			}
			seen[c.Model.ID] = true
			cands = append(cands, c)
		}
		boundID := "" // 已绑定的模型条目 ID，用于区分「复用绑定」与「新建绑定」
		var skips []SkipReason
		if sessKey != "" {
			if modelID, ok := s.getSession(sessKey); ok {
				if c, ok := s.store.CandidateByID(modelID); ok {
					boundID = modelID // 绑定存在即记下，协议不匹配改选时日志才算「会话内更换模型」
					if candidateSupportsProtocol(c, protocol, responsesConvert) {
						addCand(c)
					} else {
						skips = append(skips, SkipReason{
							ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
							Reason: "绑定的模型" + skipNoResponses + "，改用其他模型",
						})
					}
				}
			}
		}
		var rule []Candidate
		var ruleSkips []SkipReason
		if isGroup {
			rule, ruleSkips = s.store.GroupCandidatesDetailed(group.ID, now)
		} else {
			rule, ruleSkips = s.store.CandidatesDetailed(req.Model, now, false)
		}
		skips = append(skips, ruleSkips...)
		protocolSkipped := 0
		for _, c := range rule {
			if !candidateSupportsProtocol(c, protocol, responsesConvert) {
				protocolSkipped++
				skips = append(skips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: skipNoResponses,
				})
				continue
			}
			addCand(c)
		}
		// 请求指定的模型全部不支持该协议时，改用其他支持该协议的模型（不跨协议硬发）。
		// 组请求只在组内兜底；直接模型名沿用全局兜底。
		// 仅当确有模型因协议被剔除才触发，避免掩盖「模型未配置/静音/超限」等真实原因。
		if protocol == "responses" && !isGroup && len(cands) == 0 && protocolSkipped > 0 {
			all, _ := s.store.CandidatesDetailed("", now, true)
			for _, c := range all {
				if candidateSupportsProtocol(c, protocol, responsesConvert) {
					addCand(c)
				}
			}
		}
		if len(cands) == 0 {
			msg := fmt.Sprintf("模型 %q 当前没有可用上游（未配置、处于静音时段、已达每日上限或并行已满）", req.Model)
			if protocol == "responses" && !responsesConvert && protocolSkipped > 0 {
				msg = fmt.Sprintf("模型 %q 不支持 Responses 协议，且没有其他可用模型能承接该协议（可在设置中开启 Responses 协议转换，或改用 Chat Completions 入口）", req.Model)
				if isGroup {
					msg = fmt.Sprintf("路由组 %q 中没有支持 Responses 协议的模型（可在设置中开启 Responses 协议转换，或改用 Chat Completions 入口）", req.Model)
				}
			} else if isGroup {
				msg = fmt.Sprintf("路由组 %q 当前没有可用模型（组内未配置、处于静音时段、已达每日上限或并行已满）", req.Model)
			}
			s.logFail(LogEntry{
				Level: "error", Kind: kindNoCandidate, Status: http.StatusNotFound,
				KeyID: keyID, Model: req.Model, Message: msg,
			})
			writeErr(w, http.StatusNotFound, msg)
			return
		}

		lastErr := fmt.Sprintf("模型 %q 无候选", req.Model)
		var failures []string         // 逐个上游的失败原因，全部失败时一并写进日志
		var attemptSkips []SkipReason // 循环中实际尝试过但让位的候选及原因（随会话日志落盘）
		// 候选成功后的会话绑定（普通透传与 WorkBuddy 适配两条路径共用）
		bind := func(c Candidate, status int) {
			if sessKey == "" {
				return
			}
			s.setSession(sessKey, c.Model.ID, keyID, req.Model) // 成功即绑定/续期
			// 只在「新建会话」或「会话内换模型」时记日志；同一会话复用绑定不重复记录
			if boundID != c.Model.ID {
				s.logSession(s.buildSessionLog(LogEntry{
					KeyID:      keyID,
					ProviderID: c.Provider.ID,
					Model:      c.Model.Name,
					SessionID:  sid,
					Protocol:   protocol,
					Requested:  req.Model,
					Switched:   boundID != "",
					Status:     status,
				}, c, boundID, cands, skips, attemptSkips, isGroup))
			}
		}
		for _, c := range cands {
			// 原子占位：候选列表是乐观筛选的，多个并发请求可能同时看到"还有空位"。
			// 真正发请求前再抢一次，抢不到说明该模型已满，跳到下一个候选。
			if !s.store.TryAcquire(c.Model.ID) {
				lastErr = fmt.Sprintf("%s 已达并行上限 %d", c.Model.Name, c.Model.MaxConcurrent)
				failures = append(failures, lastErr)
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: fmt.Sprintf("尝试时并行已满（上限 %d），让位给下一候选", c.Model.MaxConcurrent),
				})
				continue
			}
			// 计时从本次上游尝试开始，失败换候选时重新起算，避免把前一个候选
			// 的排队/失败耗时算进当前请求的 TTFT
			tm := newRequestTiming()
			// WorkBuddy 候选走适配分支：归一化出站、专用请求头、token 续期、
			// 流式节拍过滤与非流式聚合都在 wbAttempt 里完成；/responses 请求由
			// wbAttempt 翻译成 chat 协议发上游，回程再渲染回 responses 形态
			if e, _ := wbOf(c.Provider); e != nil {
				sendBody := body
				if isGroup {
					sendBody = withModel(body, c.Model.Name)
				}
				var sm usageSample
				sm.EffortDeclared, sm.EffortSent = effort, effort
				if err := s.wbAttempt(w, r, c, sendBody, keyID, suffix == "/responses", tm, &sm); err != nil {
					lastErr = err.Error()
					failures = append(failures, fmt.Sprintf("%s: %v", c.Provider.Name, err))
					attemptSkips = append(attemptSkips, SkipReason{
						ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
						Reason: "尝试失败：" + truncateRunes(lastErr, 160),
					})
					continue // 名额释放与失败日志由 wbAttempt 处理
				}
				bind(c, http.StatusOK)
				s.store.AddCallUsage(sid, req.Model, &c.Model, keyID, sm)
				s.addSessionUsage(sessKey, sm)
				return
			}
			// Zed 候选走原生客户端适配分支：信封装配、JWT 懒重铸/强制重铸、
			// 上游事件流翻译回客户端协议都在 zedAttempt 里完成（chat 与 responses
			// 两种接入协议均承接，内部按模型前缀选 Zed 线协议）
			if zedOf(c.Provider) != nil {
				sendBody := body
				if isGroup {
					sendBody = withModel(body, c.Model.Name)
				}
				var sm usageSample
				sm.EffortDeclared, sm.EffortSent = effort, effort
				if err := s.zedAttempt(w, r, c, sendBody, keyID, suffix == "/responses", tm, &sm); err != nil {
					lastErr = err.Error()
					failures = append(failures, fmt.Sprintf("%s: %v", c.Provider.Name, err))
					attemptSkips = append(attemptSkips, SkipReason{
						ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
						Reason: "尝试失败：" + truncateRunes(lastErr, 160),
					})
					continue // 名额释放与失败日志由 zedAttempt 处理
				}
				bind(c, http.StatusOK)
				s.store.AddCallUsage(sid, req.Model, &c.Model, keyID, sm)
				s.addSessionUsage(sessKey, sm)
				return
			}
			// Command Code 候选走 CLI 线协议：出站翻译成 /alpha/generate 的
			// envelope，回程把逐行事件流翻回 chat / responses 协议（两种接入
			// 协议都在适配层内完成，不需要全局 responses 转换开关）
			if ccOf(c.Provider) != nil {
				sendBody := body
				if isGroup {
					sendBody = withModel(body, c.Model.Name)
				}
				var sm usageSample
				sm.EffortDeclared, sm.EffortSent = effort, effort
				if err := s.ccAttempt(w, r, c, sendBody, keyID, suffix == "/responses", tm, &sm); err != nil {
					lastErr = err.Error()
					failures = append(failures, fmt.Sprintf("%s: %v", c.Provider.Name, err))
					attemptSkips = append(attemptSkips, SkipReason{
						ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
						Reason: "尝试失败：" + truncateRunes(lastErr, 160),
					})
					continue // 名额释放与失败日志由 ccAttempt 处理
				}
				bind(c, http.StatusOK)
				s.store.AddCallUsage(sid, req.Model, &c.Model, keyID, sm)
				s.addSessionUsage(sessKey, sm)
				return
			}
			sendBody := body
			if isGroup {
				sendBody = withModel(body, c.Model.Name) // 仅替换顶层 model 字段的值
			}
			up, err := s.upstreamRequest(r, c.Provider, suffix, sendBody)
			if err != nil {
				s.store.Release(c.Model.ID)
				lastErr = err.Error()
				failures = append(failures, fmt.Sprintf("%s: 构造请求失败: %v", c.Provider.Name, err))
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: "尝试失败：构造请求失败: " + truncateRunes(lastErr, 160),
				})
				s.logFail(LogEntry{
					Level: "error", Kind: kindUpstream, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "构造上游请求失败", Detail: err.Error(),
				})
				continue
			}
			cli, err := s.upstreamClient(c.Provider, 0)
			if err != nil {
				s.store.Release(c.Model.ID)
				lastErr = fmt.Sprintf("%s: %v", c.Provider.Name, err)
				failures = append(failures, lastErr)
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: "尝试失败：" + truncateRunes(lastErr, 160),
				})
				s.logFail(LogEntry{
					Level: "error", Kind: kindUpstream, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "上游代理配置非法", Detail: err.Error(),
				})
				continue
			}
			resp, err := cli.Do(up)
			if err != nil {
				s.store.Release(c.Model.ID)
				lastErr = fmt.Sprintf("%s: %v", c.Provider.Name, err)
				failures = append(failures, lastErr)
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: "尝试失败：" + truncateRunes(lastErr, 160),
				})
				s.logFail(LogEntry{
					Level: "error", Kind: kindUpstream, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "连接上游失败（网络错误或超时）", Detail: err.Error(),
				})
				continue
			}
			// 任何 4xx/5xx 都换下一个候选（换一家可能就好了），失败原因带上上游错误信息
			if resp.StatusCode >= 400 {
				eb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
				resp.Body.Close()
				s.store.Release(c.Model.ID)
				lastErr = fmt.Sprintf("%s: HTTP %d: %s", c.Provider.Name, resp.StatusCode, errSnippet(eb))
				failures = append(failures, lastErr)
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: "尝试失败：" + truncateRunes(lastErr, 160),
				})
				// 429 视为明确限流：把该模型条目自动静音一段时间，后续请求直接跳过它
				if resp.StatusCode == http.StatusTooManyRequests {
					if until, ok := s.store.MuteOn429(c.Model.ID, rateLimitMute, "上游返回 429 限流"); ok {
						s.logFail(LogEntry{
							Level: "warn", Kind: kindMuted, Status: resp.StatusCode, KeyID: keyID,
							ProviderID: c.Provider.ID, Model: c.Model.Name,
							Message: fmt.Sprintf("上游返回 429，已自动静音至 %s", until.Local().Format("01-02 15:04")),
							Detail:  string(eb),
						})
						continue
					}
				}
				s.logFail(LogEntry{
					Level: "error", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode),
					Detail:  string(eb),
				})
				continue
			}
			bind(c, resp.StatusCode)
			sm := s.relay(w, resp, c.Model, tm)               // relay 返回前会释放名额
			sm.EffortDeclared, sm.EffortSent = effort, effort // 标准上游字节透传，声明即实发
			s.store.AddCallUsage(sid, req.Model, &c.Model, keyID, sm)
			s.addSessionUsage(sessKey, sm)
			return
		}
		if sessKey != "" {
			s.clearSession(sessKey) // 全部失败，解除绑定，下次会话从头按规则选
		}
		s.logFail(LogEntry{
			Level: "error", Kind: kindAllFailed, Status: http.StatusBadGateway,
			KeyID: keyID, Model: req.Model,
			Message: fmt.Sprintf("%d 个上游全部失败", len(cands)),
			Detail:  strings.Join(failures, "\n"),
		})
		writeErr(w, http.StatusBadGateway, "所有上游均失败，最后一个错误: "+lastErr)
	}
}

// candidateSupportsProtocol 判断候选能否处理该接入协议。标准 OpenAI 兼容 Provider
// 两种协议都原样透传；WorkBuddy 上游只有 chat completions，仅在开启 responses
// 协议转换时才由适配层翻译承接 /v1/responses；Zed 与 Command Code 适配层两种
// 协议恒可承接（chat 直接转换，responses 经 chat 中间形态转换）。
func candidateSupportsProtocol(c Candidate, protocol string, responsesConvert bool) bool {
	if protocol != "responses" {
		return true
	}
	if e, _ := wbOf(c.Provider); e != nil {
		return responsesConvert
	}
	return true
}

// buildSessionLog 生成会话日志：在基础字段上补选路理由、候选顺序与被剔除的原因。
// 只在新建会话或会话中途换模型时调用，所以理由只有「新会话」「会话切换」两类。
// skips 是选路规则的剔除项（静音/上限等），attemptSkips 是循环中实际尝试过但
// 让位的候选（并行抢不到名额、上游失败后降级），两者合并进 skipped 一并落盘。
func (s *Server) buildSessionLog(e LogEntry, chosen Candidate, boundID string, cands []Candidate, skips, attemptSkips []SkipReason, isGroup bool) LogEntry {
	// 候选顺序，带上优先级，便于看清"为什么是它"
	for _, c := range cands {
		e.Candidates = append(e.Candidates, fmt.Sprintf("%s/%s (优先级 %d)", c.Provider.Name, c.Model.Name, c.Model.Priority))
	}
	e.Skipped = append(append([]SkipReason(nil), skips...), attemptSkips...)

	switch {
	case boundID != "" && boundID != chosen.Model.ID:
		e.Switched = true
		e.Reason = fmt.Sprintf("会话内更换模型：原绑定上游本次不可用，按优先级切换到最高可用候选（%d）", chosen.Model.Priority)
	case isGroup:
		e.Reason = fmt.Sprintf("新会话，路由组 %q：组内优先级最高（%d）", e.Requested, chosen.Model.Priority)
	default:
		e.Reason = fmt.Sprintf("新会话，按优先级选中最高可用候选（%d）", chosen.Model.Priority)
	}
	if len(attemptSkips) > 0 {
		e.Reason += fmt.Sprintf("；前 %d 个候选尝试失败已降级（原因见已剔除）", len(attemptSkips))
	}
	// 指定模型全部不支持 responses 协议时已回落到其他模型，理由里要点明
	if !isGroup && e.Requested != "" && chosen.Model.Name != e.Requested {
		e.Reason += fmt.Sprintf("；请求的模型 %q 不支持该协议或无可用条目，已改用支持该协议的其他模型", e.Requested)
	}

	// 把「与选中项同优先级、但因并行已满而让位」的情况说清楚
	for _, sk := range skips {
		if strings.HasPrefix(sk.Reason, "并行已满") {
			e.Reason += fmt.Sprintf("；%s 同时因 %s 未参与", sk.Model, sk.Reason)
			break
		}
	}
	if e.Switched {
		e.Message = fmt.Sprintf("会话换模型 → %s / %s", chosen.Provider.Name, chosen.Model.Name)
	} else {
		e.Message = fmt.Sprintf("建立会话 → %s / %s", chosen.Provider.Name, chosen.Model.Name)
	}
	return e
}

// withModel 只替换请求体顶层 model 字段的值，其余字段以 RawMessage 原样保留（路由组请求用）
func withModel(body []byte, name string) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	top["model"], _ = json.Marshal(name)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// upstreamRequest 构造发往上游的请求：按协议选择基址，复用客户端的头（去掉鉴权/逐跳头），换成 provider 的密钥
func (s *Server) upstreamRequest(r *http.Request, p Provider, suffix string, body []byte) (*http.Request, error) {
	base := p.BaseURL
	if suffix == "/responses" && p.ResponsesURL != "" {
		base = p.ResponsesURL
	}
	url := strings.TrimRight(base, "/") + suffix
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vv := range r.Header {
		if skipReqHeader(k) {
			continue
		}
		for _, v := range vv {
			up.Header.Add(k, v)
		}
	}
	if up.Header.Get("Content-Type") == "" {
		up.Header.Set("Content-Type", "application/json")
	}
	if up.Header.Get("Accept-Encoding") == "" {
		// 不让 transport 透明加 gzip 再解压，保证字节级透传
		up.Header.Set("Accept-Encoding", "identity")
	}
	switch {
	case p.APIKey != "":
		up.Header.Set("Authorization", "Bearer "+p.APIKey)
	case s.apiToken != "":
		up.Header.Del("Authorization") // 不把本网关的令牌泄漏给上游
	}
	return up, nil
}

func skipReqHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	if k == "Authorization" || k == "Host" || k == "Content-Length" {
		return true
	}
	for _, h := range hopHeaders {
		if k == h {
			return true
		}
	}
	return false
}

// sseSniffer 透传 SSE 的同时保留一份滚动尾部（不改任何字节），流结束后从中提取 usage
type sseSniffer struct {
	w    io.Writer
	tail []byte
}

func (s *sseSniffer) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.tail = append(s.tail, p[:n]...)
	if len(s.tail) > 8192 {
		s.tail = s.tail[len(s.tail)-8192:]
	}
	return n, err
}

// requestTiming 记录一次上游调用的计时点：发起时刻与首个响应字节到达时刻。
// 首字节（TTFT）从发起请求算起，包含排队、预填充与上游出字延迟。
type requestTiming struct {
	start time.Time
	first time.Time // 零值 = 还没收到任何响应字节
}

func newRequestTiming() *requestTiming { return &requestTiming{start: time.Now()} }

func (t *requestTiming) markFirst() {
	if t.first.IsZero() {
		t.first = time.Now()
	}
}

// sample 合成一次调用的用量与耗时。流式的生成耗时从首字节算到流结束（排除排队与
// 预填充），非流式只有端到端耗时；两者都由 tokPerSec 统一换算。
func (t *requestTiming) sample(u Usage, stream bool) usageSample {
	end := time.Now()
	sm := usageSample{Usage: u, Total: end.Sub(t.start), Stream: stream}
	if !t.first.IsZero() {
		sm.TTFT = t.first.Sub(t.start)
		if stream {
			sm.Gen = end.Sub(t.first)
		}
	}
	if sm.Gen <= 0 {
		sm.Gen = sm.Total
	}
	return sm
}

// usageSample 一次调用的用量 + 吞吐计时
type usageSample struct {
	Usage
	Stream bool          // 是否流式响应（决定 TTFT 是否有意义）
	TTFT   time.Duration // 首字节耗时
	Gen    time.Duration // 生成耗时
	Total  time.Duration // 端到端耗时

	// 思考等级：EffortDeclared 是客户端请求体里的声明值，EffortSent 是实际写进
	// 出站请求的档位（适配层解析后覆盖；标准上游字节透传，两者相同）
	EffortDeclared string
	EffortSent     string
}

// tokPerSec 输出速度；没有输出 token 或耗时时返回 0
func (u usageSample) tokPerSec() float64 {
	if u.Completion <= 0 || u.Gen <= 0 {
		return 0
	}
	return float64(u.Completion) / u.Gen.Seconds()
}

// applySample 用本次计时聚合结果更新用量，同时保留适配层已写入的思考等级
// （tm.sample 只负责 usage/计时，整体赋值会抹掉档位信息）。
func (u *usageSample) applySample(s usageSample) {
	s.EffortDeclared, s.EffortSent = u.EffortDeclared, u.EffortSent
	*u = s
}

// firstByteReader 在首次从上游读到字节时给 requestTiming 打点，不改变字节流。
type firstByteReader struct {
	r io.Reader
	t *requestTiming
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.t.markFirst()
	}
	return n, err
}

// usageDetail usage 的分项对象。CachedTokens 用指针区分「字段缺失」与「显式 0」：
// 部分上游会同时在 completion_tokens_details 里返回 cached_tokens:0，不能拿它当
// 提示缓存（真正的值在 prompt_tokens_details / input_tokens_details）。
type usageDetail struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

// usagePayload 一次调用的 usage 对象，兼容 chat completions 与 responses 两套字段名。
type usagePayload struct {
	PromptTokens         int64        `json:"prompt_tokens"`
	CompletionTokens     int64        `json:"completion_tokens"`
	PromptTokensDetails  *usageDetail `json:"prompt_tokens_details"`
	InputTokens          int64        `json:"input_tokens"`
	OutputTokens         int64        `json:"output_tokens"`
	InputTokensDetails   *usageDetail `json:"input_tokens_details"`
	PromptCacheHitTokens *int64       `json:"prompt_cache_hit_tokens"` // DeepSeek 风格
	CacheReadInputTokens *int64       `json:"cache_read_input_tokens"` // Anthropic 风格
	CachedTokens         *int64       `json:"cached_tokens"`           // 顶层兜底
}

// parseUsageObject 解析 usage JSON 对象为记账用量。缓存命中取第一个存在的
// prompt/input 分项，再依次回落 DeepSeek / Anthropic / 顶层字段。
func parseUsageObject(raw []byte) Usage {
	if len(raw) == 0 {
		return Usage{}
	}
	var v usagePayload
	if json.Unmarshal(raw, &v) != nil {
		return Usage{}
	}
	u := Usage{Prompt: v.PromptTokens, Completion: v.CompletionTokens}
	if u.Prompt == 0 && u.Completion == 0 {
		u.Prompt, u.Completion = v.InputTokens, v.OutputTokens
	}
	switch {
	case v.PromptTokensDetails != nil && v.PromptTokensDetails.CachedTokens != nil:
		u.Cached = *v.PromptTokensDetails.CachedTokens
	case v.InputTokensDetails != nil && v.InputTokensDetails.CachedTokens != nil:
		u.Cached = *v.InputTokensDetails.CachedTokens
	case v.PromptCacheHitTokens != nil:
		u.Cached = *v.PromptCacheHitTokens
	case v.CacheReadInputTokens != nil:
		u.Cached = *v.CacheReadInputTokens
	case v.CachedTokens != nil:
		u.Cached = *v.CachedTokens
	}
	return u
}

// extractStreamUsage 从 SSE 尾部提取最后一个非空 usage 对象里的 token 数。
// 按 data: 行逆序解析，避免正文里出现的 "usage" 文本被误当用量；单行被尾部
// 8KB 截断时退回花括号配对，保证 responses 的大事件也能取到 usage。
func extractStreamUsage(tail []byte) Usage {
	for end := len(tail); end > 0; {
		i := bytes.LastIndexByte(tail[:end], '\n')
		line := tail[i+1 : end]
		if i < 0 { // tail 从某行中间开始：这一段仍尝试解析，失败会自然跳过
			line = tail[:end]
			end = 0
		} else {
			end = i
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		d, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			// tail 只保留最后 8KB，超大的 response.completed 事件可能连 data: 前缀
			// 都被截掉。对残余片段仍做一次 usage 对象配对（正文里的 "usage" 文本
			// 因为没有 prompt/completion 字段，解析后通常是全 0，不影响正常流）。
			if obj := lastUsageObject(line); obj != nil {
				return parseUsageObject(obj)
			}
			continue
		}
		d = bytes.TrimSpace(d)
		if len(d) == 0 || bytes.Equal(d, []byte("[DONE]")) {
			continue
		}
		if obj := usageObjectInPayload(d); obj != nil {
			return parseUsageObject(obj)
		}
	}
	return Usage{}
}

// usageObjectInPayload 从一条 SSE data 载荷取 usage 对象：chat completions 在顶层，
// responses 事件在 response.usage。载荷是合法 JSON 但 usage 为 null/缺失时返回 nil，
// 继续向前找真正带用量的那一帧。
func usageObjectInPayload(d []byte) []byte {
	var top struct {
		Usage    json.RawMessage `json:"usage"`
		Response *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(d, &top) == nil {
		if !isNullOrEmptyJSON(top.Usage) {
			return top.Usage
		}
		if top.Response != nil && !isNullOrEmptyJSON(top.Response.Usage) {
			return top.Response.Usage
		}
		return nil
	}
	return lastUsageObject(d) // 行被 8KB 尾部截断
}

func isNullOrEmptyJSON(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("{}"))
}

// lastUsageObject 取 tail 中最后一个 "usage" 键对应的完整 JSON 对象；
// 找不到、值为 null 或对象被截断时返回 nil。
func lastUsageObject(tail []byte) []byte {
	i := bytes.LastIndex(tail, []byte(`"usage"`))
	if i < 0 {
		return nil
	}
	j := i + len(`"usage"`)
	for j < len(tail) && (tail[j] == ' ' || tail[j] == '\t' || tail[j] == '\r' || tail[j] == '\n' || tail[j] == ':') {
		j++
	}
	if j >= len(tail) || tail[j] != '{' {
		return nil // null 或残缺
	}
	depth := 0
	inStr, esc := false, false
	for k := j; k < len(tail); k++ {
		c := tail[k]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return tail[j : k+1]
			}
		}
	}
	return nil
}

// relay 把上游响应原样写回客户端，并把本次调用的用量与耗时返回给调用方
// （全局用量与会话用量的记账在 handleProxy 里统一完成）。
// 函数返回时释放该模型的并行名额（defer 保证流式/非流式/204 各路径都会释放）。
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, m Model, tm *requestTiming) usageSample {
	defer s.store.Release(m.ID)
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		if skipRespHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == 204 || resp.StatusCode == 304 {
		return tm.sample(Usage{}, false)
	}
	body := &firstByteReader{r: resp.Body, t: tm}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sn := &sseSniffer{w: flushWriter{w}}
		io.Copy(sn, body) // 流式：边收边发，字节不变，尾部嗅探 usage
		return tm.sample(extractStreamUsage(sn.tail), true)
	}
	buf, _ := io.ReadAll(body)
	var top struct {
		Usage json.RawMessage `json:"usage"`
	}
	json.Unmarshal(buf, &top)
	w.Write(buf)
	return tm.sample(parseUsageObject(top.Usage), false)
}

func skipRespHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	for _, h := range hopHeaders {
		if k == h {
			return true
		}
	}
	return false
}

// handleModels 聚合所有对外模型名，兼容 OpenAI 的 /v1/models
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkAPIToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	names := s.store.ModelNames()
	data := []map[string]any{}
	seen := map[string]bool{}
	for _, g := range s.store.GroupsList() { // 路由组名始终可见
		if seen[g.Name] {
			continue
		}
		seen[g.Name] = true
		data = append(data, map[string]any{"id": g.Name, "object": "model", "owned_by": "llmrouter"})
	}
	for _, n := range names {
		if seen[n] { // 已有同名路由组时避免重复
			continue
		}
		seen[n] = true
		data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "llmrouter"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
