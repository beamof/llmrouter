package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
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
	requested string    // 客户端请求的 model 名（auto 虚拟模型时与绑定值不同）
	firstAt   time.Time // 首次绑定时刻
	lastAt    time.Time // 最近一次成功请求时刻
	count     int       // 会话内成功请求数
	expires   time.Time
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
		lastAt: now, expires: now.Add(sessionTTL),
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
		out = append(out, sessionInfo{
			ID: id, KeyID: v.keyID, ModelID: v.modelID, Requested: v.requested,
			Count: v.count, FirstAt: v.firstAt, LastAt: v.lastAt, Expires: v.expires,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
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

		// auto 虚拟模型：对外统一声明一个名字，按全局优先级自动挑选实际模型
		autoName := s.store.AutoModelName()
		isAuto := req.Model == autoName
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
		rule, ruleSkips := s.store.CandidatesDetailed(req.Model, now, isAuto)
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
		// 仅当确有模型因协议被剔除才触发，避免掩盖「模型未配置/静音/超限」等真实原因。
		if protocol == "responses" && !isAuto && len(cands) == 0 && protocolSkipped > 0 {
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
				if isAuto {
					msg = "当前没有可用模型支持 Responses 协议（可在设置中开启 Responses 协议转换，或改用 Chat Completions 入口）"
				}
			} else if isAuto {
				msg = "当前没有可用模型（全部未配置、处于静音时段、已达每日上限或并行已满）"
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
				}, c, boundID, cands, skips, attemptSkips, isAuto))
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
			// WorkBuddy 候选走适配分支：归一化出站、专用请求头、token 续期、
			// 流式节拍过滤与非流式聚合都在 wbAttempt 里完成；/responses 请求由
			// wbAttempt 翻译成 chat 协议发上游，回程再渲染回 responses 形态
			if e, _ := wbOf(c.Provider); e != nil {
				sendBody := body
				if isAuto {
					sendBody = withModel(body, c.Model.Name)
				}
				if err := s.wbAttempt(w, r, c, sendBody, keyID, suffix == "/responses"); err != nil {
					lastErr = err.Error()
					failures = append(failures, fmt.Sprintf("%s: %v", c.Provider.Name, err))
					attemptSkips = append(attemptSkips, SkipReason{
						ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
						Reason: "尝试失败：" + truncateRunes(lastErr, 160),
					})
					continue // 名额释放与失败日志由 wbAttempt 处理
				}
				bind(c, http.StatusOK)
				return
			}
			sendBody := body
			if isAuto {
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
			resp, err := s.client.Do(up)
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
			s.relay(w, resp, c.Model, keyID) // relay 返回前会释放名额
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
// 协议转换时才由适配层翻译承接 /v1/responses。
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
func (s *Server) buildSessionLog(e LogEntry, chosen Candidate, boundID string, cands []Candidate, skips, attemptSkips []SkipReason, isAuto bool) LogEntry {
	// 候选顺序，带上优先级，便于看清"为什么是它"
	for _, c := range cands {
		e.Candidates = append(e.Candidates, fmt.Sprintf("%s/%s (优先级 %d)", c.Provider.Name, c.Model.Name, c.Model.Priority))
	}
	e.Skipped = append(append([]SkipReason(nil), skips...), attemptSkips...)

	switch {
	case boundID != "" && boundID != chosen.Model.ID:
		e.Switched = true
		e.Reason = fmt.Sprintf("会话内更换模型：原绑定上游本次不可用，按优先级切换到最高可用候选（%d）", chosen.Model.Priority)
	case isAuto:
		e.Reason = fmt.Sprintf("新会话，auto 虚拟模型：全部可用模型中优先级最高（%d）", chosen.Model.Priority)
	default:
		e.Reason = fmt.Sprintf("新会话，按优先级选中最高可用候选（%d）", chosen.Model.Priority)
	}
	if len(attemptSkips) > 0 {
		e.Reason += fmt.Sprintf("；前 %d 个候选尝试失败已降级（原因见已剔除）", len(attemptSkips))
	}
	// 指定模型全部不支持 responses 协议时已回落到其他模型，理由里要点明
	if !isAuto && e.Requested != "" && chosen.Model.Name != e.Requested {
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

// withModel 只替换请求体顶层 model 字段的值，其余字段以 RawMessage 原样保留（auto 虚拟模型用）
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
	w    flushWriter
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

var (
	rePromptTok     = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reCompletionTok = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	reInputTok      = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	reOutputTok     = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)
	reCachedTok     = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
)

// extractStreamUsage 从 SSE 尾部提取最后一个 usage 对象里的 token 数，
// 兼容 chat completions（prompt/completion_tokens）与 responses（input/output_tokens）两种字段名，
// 并提取缓存命中量（prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens）
func extractStreamUsage(tail []byte) Usage {
	i := bytes.LastIndex(tail, []byte(`"usage"`))
	if i < 0 {
		return Usage{}
	}
	seg := tail[i:]
	if len(seg) > 2048 {
		seg = seg[:2048]
	}
	grab := func(re *regexp.Regexp) int64 {
		if m := re.FindSubmatch(seg); m != nil {
			if v, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				return v
			}
		}
		return 0
	}
	u := Usage{Prompt: grab(rePromptTok), Completion: grab(reCompletionTok), Cached: grab(reCachedTok)}
	if u.Prompt == 0 && u.Completion == 0 {
		u.Prompt, u.Completion = grab(reInputTok), grab(reOutputTok)
	}
	return u
}

// relay 把上游响应原样写回客户端；非流式响应顺带读取 usage 记账。
// 函数返回时释放该模型的并行名额（defer 保证流式/非流式/204 各路径都会释放）。
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, m Model, keyID string) {
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
		s.store.AddUsage(&m, keyID, Usage{})
		return
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sn := &sseSniffer{w: flushWriter{w}}
		io.Copy(sn, resp.Body) // 流式：边收边发，字节不变，尾部嗅探 usage
		s.store.AddUsage(&m, keyID, extractStreamUsage(sn.tail))
		return
	}
	buf, _ := io.ReadAll(resp.Body)
	var u struct {
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"` // chat completions
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputTokens        int64 `json:"input_tokens"` // responses 协议
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	json.Unmarshal(buf, &u)
	var uu Usage
	if u.Usage != nil {
		uu = Usage{Prompt: u.Usage.PromptTokens, Completion: u.Usage.CompletionTokens}
		if uu.Prompt == 0 && uu.Completion == 0 {
			uu.Prompt, uu.Completion = u.Usage.InputTokens, u.Usage.OutputTokens
		}
		if u.Usage.PromptTokensDetails != nil {
			uu.Cached = u.Usage.PromptTokensDetails.CachedTokens
		} else if u.Usage.InputTokensDetails != nil {
			uu.Cached = u.Usage.InputTokensDetails.CachedTokens
		}
	}
	w.Write(buf)
	s.store.AddUsage(&m, keyID, uu)
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
	auto := s.store.AutoModelName()
	data := []map[string]any{{"id": auto, "object": "model", "owned_by": "llmrouter"}}
	for _, n := range names {
		if n == auto { // 已有同名模型路由时避免重复
			continue
		}
		data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "llmrouter"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
