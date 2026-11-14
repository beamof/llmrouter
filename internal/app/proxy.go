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

		// 路由组：组名即对外虚拟模型名，命中组时只在组内按优先级挑选实际模型。
		// 非组名一律按直连模型处理：对外名必须是唯一的 provider/model。
		group, isGroup := s.store.GroupByName(req.Model)
		isDirect := !isGroup
		effort := requestEffort(body) // 客户端声明的思考等级，随会话记录（实际出站值由适配层覆盖）
		protocol := "chat"
		if suffix == "/responses" {
			protocol = "responses"
		}
		// responses 协议转换开关（默认关闭）：关闭时仅支持 chat 的上游（WorkBuddy）
		// 不承接 /responses；组请求会在组内改选原生支持 responses 的模型。
		responsesConvert := s.store.ResponsesConvertEnabled()

		// 会话绑定只影响路由组选路；直连模型名已唯一，绑定仅用于展示与统计。
		// 绑定键含接入协议：同一会话混用两种协议时各自绑定。
		// 未声明 X-Session-ID 时按客户端 IP 派生固定 UUID 作为会话 ID。
		sessKey := ""
		sid := r.Header.Get("X-Session-ID")
		if sid == "" {
			sid = derivedSessionID(r)
		}
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
				boundID = modelID // 绑定存在即记下，协议不匹配改选时日志才算「会话内更换模型」
				if !isDirect {
					if c, ok := s.store.CandidateByID(modelID); ok {
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
		}
		if isGroup {
			rule, ruleSkips := s.store.GroupCandidatesDetailed(group.ID, now)
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
			if len(cands) == 0 {
				msg := fmt.Sprintf("路由组 %q 当前没有可用模型（组内未配置、处于静音时段、已达每日上限或并行已满）", req.Model)
				if protocol == "responses" && !responsesConvert && protocolSkipped > 0 {
					msg = fmt.Sprintf("路由组 %q 中没有支持 Responses 协议的模型（可在设置中开启 Responses 协议转换，或改用 Chat Completions 入口）", req.Model)
				}
				s.logFail(LogEntry{
					Level: "error", Kind: kindNoCandidate, Status: http.StatusNotFound,
					KeyID: keyID, Model: req.Model, Message: msg,
				})
				writeErr(w, http.StatusNotFound, msg)
				return
			}
		} else {
			// 直连：唯一命中一条配置，不做任何兜底。
			lookup := s.store.DirectCandidate(req.Model, now)
			switch {
			case lookup.Ambiguous:
				msg := fmt.Sprintf("模型 %q 存在多条同名配置（同一 provider 下模型名重复），请先在后台清理重复条目", req.Model)
				s.logFail(LogEntry{
					Level: "error", Kind: kindNoCandidate, Status: http.StatusConflict,
					KeyID: keyID, Model: req.Model, Message: msg,
				})
				writeErr(w, http.StatusConflict, msg)
				return
			case !lookup.Found:
				msg := fmt.Sprintf("未知模型 %q：直连请使用 provider/model 形式（如 glm/glm-5.3），路由组名可直接使用", req.Model)
				s.logFail(LogEntry{
					Level: "warn", Kind: kindNoCandidate, Status: http.StatusNotFound,
					KeyID: keyID, Model: req.Model, Message: msg,
				})
				writeErr(w, http.StatusNotFound, msg)
				return
			case lookup.Reason != "":
				msg := fmt.Sprintf("模型 %q 当前不可用：%s", req.Model, lookup.Reason)
				s.logFail(LogEntry{
					Level: "warn", Kind: kindNoCandidate, Status: http.StatusTooManyRequests,
					KeyID: keyID, Model: req.Model, Message: msg,
				})
				writeErr(w, http.StatusTooManyRequests, msg)
				return
			}
			c := lookup.Candidate
			if !s.candidateSupportsProtocol(c, protocol) {
				msg := fmt.Sprintf("模型 %q 不支持当前接入协议 /v1%s", req.Model, suffix)
				if protocol == "responses" && !responsesConvert {
					msg += "（可在设置中开启 Responses 协议转换）"
				}
				s.logFail(LogEntry{
					Level: "warn", Kind: kindBadRequest, Status: http.StatusBadRequest,
					KeyID: keyID, Model: req.Model, Message: msg,
				})
				writeErr(w, http.StatusBadRequest, msg)
				return
			}
			addCand(c)
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
			// 真正发请求前再抢一次，抢不到说明该模型已满。
			if !s.store.TryAcquire(c.Model.ID) {
				lastErr = fmt.Sprintf("%s 已达并行上限 %d", c.Model.Name, c.Model.MaxConcurrent)
				failures = append(failures, lastErr)
				attemptSkips = append(attemptSkips, SkipReason{
					ModelID: c.Model.ID, Model: c.Model.Name, ProviderID: c.Provider.ID,
					Reason: fmt.Sprintf("尝试时并行已满（上限 %d），让位给下一候选", c.Model.MaxConcurrent),
				})
				if isDirect {
					msg := fmt.Sprintf("模型 %q 当前不可用：%s", req.Model, attemptSkips[len(attemptSkips)-1].Reason)
					s.logFail(LogEntry{
						Level: "warn", Kind: kindNoCandidate, Status: http.StatusTooManyRequests,
						KeyID: keyID, Model: req.Model, Message: msg,
					})
					writeErr(w, http.StatusTooManyRequests, msg)
					return
				}
				continue
			}
			// 计时从本次上游尝试开始，失败换候选时重新起算，避免把前一个候选
			// 的排队/失败耗时算进当前请求的 TTFT
			tm := upstream.NewTiming()
			// 组请求与直连请求都要把对外名（组名 / provider/model）改写为上游模型名，
			// 只替换顶层 model 字段的值。
			sendBody := withModel(body, c.Model.Name)
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
				s.logAttemptFailure(adapter, c, err, keyID)
				if isDirect {
					// 直连不做任何兜底：上游原始状态码/响应体直接回给客户端；
					// 网络等无法透传的错误回退为网关 502。
					if !writeUpstreamError(w, err) {
						writeErr(w, http.StatusBadGateway, "上游失败: "+lastErr)
					}
					return
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

// logAttemptFailure 记录一次候选失败：私有适配器自行记日志；标准上游由网关统一归因，
// 429 还会自动静音。直连与路由组共用。
func (s *Server) logAttemptFailure(adapter upstream.Adapter, c Candidate, err error, keyID string) {
	if _, isStandard := adapter.(*upstream.Standard); !isStandard {
		return
	}
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
				return
			}
		}
		s.logFail(LogEntry{
			Level: "error", Kind: kindUpstream, Status: se.Status, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("上游返回 HTTP %d", se.Status),
			Detail:  string(se.Body),
		})
		return
	}
	s.logFail(LogEntry{
		Level: "error", Kind: kindUpstream, KeyID: keyID,
		ProviderID: c.Provider.ID, Model: c.Model.Name,
		Message: "连接上游失败（网络错误或超时）", Detail: err.Error(),
	})
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
	case boundID != "" && boundID != chosen.Model.ID && isGroup:
		e.Switched = true
		e.Reason = fmt.Sprintf("会话内更换模型：原绑定上游本次不可用，按优先级切换到最高可用候选（%d）", chosen.Model.Priority)
	case boundID != "" && boundID != chosen.Model.ID:
		e.Reason = fmt.Sprintf("会话内模型条目已变更，直连模型 %s", e.Requested)
	case isGroup:
		e.Reason = fmt.Sprintf("新会话，路由组 %q：组内优先级最高（%d）", e.Requested, chosen.Model.Priority)
	default:
		e.Reason = fmt.Sprintf("新会话，直连模型 %s", e.Requested)
	}
	if len(attemptSkips) > 0 {
		e.Reason += fmt.Sprintf("；前 %d 个候选尝试失败已降级（原因见已剔除）", len(attemptSkips))
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

// withModel 只替换请求体顶层 model 字段的值，其余字段以 RawMessage 原样保留
// （组请求把组名、直连请求把 provider/model 统一改写为上游模型名）。
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

// handleModels 聚合所有对外模型名，兼容 OpenAI 的 /v1/models。
// 路由组名原样输出；直连模型输出唯一的 provider/model 名，并带各自的上下文声明。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkAPIToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	// modelItem 一条对外模型：组名与直连模型名形状一致。声明过上下文窗口时同时输出
	// context_length（OpenRouter 口径）与 max_model_len（vLLM 口径），认识哪个的客户端
	// 都能读到；未声明的条目不带这两个字段，客户端回退到自身默认值。
	modelItem := func(name string, contextLength int64) map[string]any {
		item := map[string]any{"id": name, "object": "model", "owned_by": "llmrouter"}
		if contextLength > 0 {
			item["context_length"] = contextLength
			item["max_model_len"] = contextLength
		}
		return item
	}
	data := []map[string]any{}
	seen := map[string]bool{}
	for _, g := range s.store.GroupsList() { // 路由组名始终可见
		if seen[g.Name] {
			continue
		}
		seen[g.Name] = true
		data = append(data, modelItem(g.Name, s.store.ContextLength(g.Name)))
	}
	for _, m := range s.store.ExposedModels() { // 直连模型名已唯一，不再按名字去重合并
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		data = append(data, modelItem(m.Name, m.ContextLength))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
