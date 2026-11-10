package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"net/http"
	"strings"
	"time"
)

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
					if s.candidateSupportsProtocol(c, protocol) {
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
			if !s.candidateSupportsProtocol(c, protocol) {
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
				if s.candidateSupportsProtocol(c, protocol) {
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
			tm := upstream.NewTiming()
			sendBody := body
			if isGroup {
				sendBody = withModel(body, c.Model.Name) // 仅替换顶层 model 字段的值
			}
			var sm usageSample
			sm.EffortDeclared, sm.EffortSent = effort, effort
			adapter := s.registry().For(c.Provider)
			err := adapter.Attempt(&upstream.Attempt{
				Writer: w, Request: r, Candidate: c, Body: sendBody,
				KeyID: keyID, Protocol: domain.Protocol(protocol), Timing: tm, Usage: &sm,
			})
			if err != nil {
				lastErr = err.Error()
				failures = append(failures, fmt.Sprintf("%s: %v", c.Provider.Name, err))
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: "尝试失败：" + truncateRunes(lastErr, 160),
				})
				// 适配器自行记录失败日志；标准上游的失败由网关统一归因与静音。
				if _, isStandard := adapter.(*upstream.Standard); isStandard {
					var se *upstream.StatusError
					if errors.As(err, &se) {
						if se.Status == http.StatusTooManyRequests {
							if until, ok := s.store.MuteOn429(c.Model.ID, rateLimitMute, "上游返回 429 限流"); ok {
								s.logFail(LogEntry{
									Level: "warn", Kind: kindMuted, Status: se.Status, KeyID: keyID,
									ProviderID: c.Provider.ID, Model: c.Model.Name,
									Message: fmt.Sprintf("上游返回 429，已自动静音至 %s", until.Local().Format("01-02 15:04")),
									Detail:  string(se.Body),
								})
								continue
							}
						}
						s.logFail(LogEntry{
							Level: "error", Kind: kindUpstream, Status: se.Status, KeyID: keyID,
							ProviderID: c.Provider.ID, Model: c.Model.Name,
							Message: fmt.Sprintf("上游返回 HTTP %d", se.Status),
							Detail:  string(se.Body),
						})
					} else {
						s.logFail(LogEntry{
							Level: "error", Kind: kindUpstream, KeyID: keyID,
							ProviderID: c.Provider.ID, Model: c.Model.Name,
							Message: "连接上游失败（网络错误或超时）", Detail: err.Error(),
						})
					}
				}
				continue
			}
			bind(c, http.StatusOK)
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
func (s *Server) candidateSupportsProtocol(c Candidate, protocol string) bool {
	return s.registry().For(c.Provider).CanServe(domain.Protocol(protocol))
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
