package main

// ─────────────────── Zed 线协议：信封装配、SSE 回程翻译与候选处理 ───────────────────
//
// Zed /completions 的请求体是一层自有信封：
//
//	{thread_id, prompt_id, intent:"user_prompt", provider, model, provider_request}
//
// provider_request 是该模型原生线协议的请求体（gpt-* → OpenAI Responses，
// claude-* → Anthropic Messages）。响应是 Zed 自有 SSE：每帧 {"status":…,"event":…}，
// status 帧汇报进度/终态（stream_ended / failed），event 帧携带 provider 原生事件。
// 本文件负责装配信封、翻译回程，以及 zedAttempt（候选循环的 Zed 分支，
// 结构与 wbAttempt 一致：懒重铸 + 401/403 强制重铸重试一次 + 流式直翻/非流式聚合）。

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// zedWirePlan 一次请求的 Zed 线协议形态
type zedWirePlan struct {
	provider string // zedProviderOpenAI / zedProviderAnthropic
	model    string
}

// zedUUID 生成 UUIDv4 文本（信封的 thread_id / prompt_id）
func zedUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// zedCompletionsHeader /completions 请求头：整体重写，不透传客户端任何头，
// 完整伪装原生 Zed 客户端（参照 ccLoad injectZedResponsesHeaders）。
func zedCompletionsHeader(cred *ZedCred) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set("User-Agent", zedUserAgent())
	h.Set("x-zed-version", zedVersion)
	h.Set("x-zed-client-supports-status-messages", "true")
	h.Set("x-zed-client-supports-stream-ended-request-completion-status", "true")
	if cred.SystemID != "" {
		h.Set("x-zed-system-id", cred.SystemID)
	}
	return h
}

// ── 请求侧：装配信封 ──────────────────────────────────────────────────────

// zedBuildEnvelope 把客户端请求（chat 或 responses 形态）装配成 Zed 信封。
// gpt 线路：responses 客户端直接归一化原始 body（保留 Codex 语义，避免两次有损
// 转换），chat 客户端先转 responses；claude 线路：统一经 chat 中间形态转 Anthropic。
func zedBuildEnvelope(sendBody []byte, responsesMode bool, model string) ([]byte, *zedWirePlan, error) {
	provider, ok := zedProviderForModel(model)
	if !ok {
		return nil, nil, fmt.Errorf("模型 %q 不在 Zed 支持范围（gpt-*/claude-*）", model)
	}
	plan := &zedWirePlan{provider: provider, model: model}
	var providerRequest map[string]any
	if provider == zedProviderAnthropic {
		chatBytes := sendBody
		if responsesMode {
			chat, err := wbResponsesToChat(sendBody)
			if err != nil {
				return nil, nil, err
			}
			if chatBytes, err = json.Marshal(chat); err != nil {
				return nil, nil, err
			}
		}
		var chatBody map[string]any
		if err := json.Unmarshal(chatBytes, &chatBody); err != nil || chatBody == nil {
			return nil, nil, fmt.Errorf("请求体不是合法 JSON 对象")
		}
		explicitMax := false
		for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if _, ok := chatBody[k]; ok {
				explicitMax = true
			}
		}
		anth, err := zedChatToAnthropic(chatBody, model)
		if err != nil {
			return nil, nil, err
		}
		if err := zedFinalizeAnthropic(anth, explicitMax); err != nil {
			return nil, nil, err
		}
		providerRequest = anth
	} else {
		var respBody map[string]any
		if responsesMode {
			if err := json.Unmarshal(sendBody, &respBody); err != nil || respBody == nil {
				return nil, nil, fmt.Errorf("/responses 请求体不是合法 JSON 对象")
			}
		} else {
			var chatBody map[string]any
			if err := json.Unmarshal(sendBody, &chatBody); err != nil || chatBody == nil {
				return nil, nil, fmt.Errorf("请求体不是合法 JSON 对象")
			}
			r, err := zedChatToResponses(chatBody, model)
			if err != nil {
				return nil, nil, err
			}
			respBody = r
		}
		respBody["model"] = model
		zedNormalizeResponses(respBody)
		providerRequest = respBody
	}
	envelope := map[string]any{
		"thread_id": zedUUID(), "prompt_id": zedUUID(), "intent": "user_prompt",
		"provider": provider, "model": model,
		"provider_request": providerRequest,
	}
	b, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	return b, plan, nil
}

// zedNormalizeResponses gpt 线路的 provider_request 归一化（参照 ccLoad 实测约束）：
// 恒为流式、input 字符串转条目数组、agent_message/developer 角色折算、
// 剥离 encrypted_content / compaction / 无摘要的 reasoning、include 去掉 reasoning.*。
func zedNormalizeResponses(body map[string]any) {
	body["stream"] = true // Zed 拥有流式，provider_request 恒带 stream:true
	switch in := body["input"].(type) {
	case string:
		body["input"] = []any{zedResponsesMessageItem("user", in)}
	case []any:
		body["input"] = zedNormalizeResponsesItems(in)
	}
	if inc := zedSlice(body["include"]); len(inc) > 0 {
		kept := make([]any, 0, len(inc))
		for _, v := range inc {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "reasoning.") {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(body, "include")
		} else {
			body["include"] = kept
		}
	}
	zedStripEncrypted(body)
}

// zedResponsesMessageItem 构造一个 message 形态的 input 条目
func zedResponsesMessageItem(role, text string) map[string]any {
	return map[string]any{"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func zedNormalizeResponsesItems(items []any) []any {
	out := make([]any, 0, len(items))
	for _, raw := range items {
		item := zedMap(raw)
		if item == nil {
			out = append(out, raw)
			continue
		}
		for k, v := range item { // 删 null 值字段（Zed 后端对 null 敏感）
			if v == nil {
				delete(item, k)
			}
		}
		switch zedStr(item["type"]) {
		case "compaction":
			continue // 上下文压缩条目：无状态转发用不上，丢弃
		case "reasoning":
			if !zedReasoningHasSummary(item) {
				continue // 无摘要的推理条目不可回放，丢弃
			}
		case "agent_message":
			item["type"] = "message"
			item["role"] = "user"
			delete(item, "author")
			delete(item, "recipient")
			zedNormalizeItemContent(item, "user")
		case "message":
			role := zedStr(item["role"])
			if role == "developer" {
				item["role"] = "system"
				role = "system"
			}
			zedNormalizeItemContent(item, role)
		}
		out = append(out, item)
	}
	return out
}

// zedNormalizeItemContent 条目 content 归一化：字符串转类型化部件数组；
// assistant 的纯文本用 output_text，其余 input_text。
func zedNormalizeItemContent(item map[string]any, role string) {
	switch c := item["content"].(type) {
	case string:
		typ := "input_text"
		if role == "assistant" {
			typ = "output_text"
		}
		item["content"] = []any{map[string]any{"type": typ, "text": c}}
	case []any:
		parts := make([]any, 0, len(c))
		for _, p := range c {
			pm := zedMap(p)
			if pm != nil && zedStr(pm["type"]) == "encrypted_content" {
				// 密文部件降级为普通文本部件（保留内容、改换形态，与 ccLoad 一致）
				if enc := zedStr(pm["encrypted_content"]); enc != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": enc})
					continue
				}
			}
			parts = append(parts, p)
		}
		item["content"] = parts
	}
}

func zedReasoningHasSummary(item map[string]any) bool {
	for _, raw := range zedSlice(item["summary"]) {
		if p := zedMap(raw); p != nil && strings.TrimSpace(zedStr(p["text"])) != "" {
			return true
		}
	}
	return false
}

// zedStripEncrypted 递归删除所有 encrypted_content 字段
func zedStripEncrypted(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "encrypted_content")
		for _, vv := range t {
			zedStripEncrypted(vv)
		}
	case []any:
		for _, vv := range t {
			zedStripEncrypted(vv)
		}
	}
}

// zedFinalizeAnthropic claude 线路的 provider_request 收尾（参照 ccLoad 踩坑规则）：
// 不带 stream 标志（Zed 拥有流）；system 字符串转文本块；max_tokens 默认 8192
// （zedChatToAnthropic 已设）；显式 max_tokens 与 thinking 预算冲突时报错，
// 未显式给时按整份默认预算抬高，避免把可见输出截断到个位数 token。
func zedFinalizeAnthropic(anth map[string]any, explicitMax bool) error {
	delete(anth, "stream")
	if s, ok := anth["system"].(string); ok {
		anth["system"] = []any{map[string]any{"type": "text", "text": s}}
	}
	if budget, ok := zedInt(zedMap(anth["thinking"])["budget_tokens"]); ok && budget > 0 {
		maxTokens, _ := zedInt(anth["max_tokens"])
		if maxTokens <= budget {
			if explicitMax {
				return fmt.Errorf("max_tokens 必须大于 thinking.budget_tokens（%d ≤ %d）", maxTokens, budget)
			}
			anth["max_tokens"] = float64(budget + zedAnthropicDefaultMaxTokens)
		}
	}
	return nil
}

// ── 回程侧：Zed SSE 帧分类 ────────────────────────────────────────────────

type zedFrameClass int

const (
	zedFrameSkip       zedFrameClass = iota // 注释行 / 无法解析的行
	zedFrameInProgress                      // 纯 status 进行中帧（queued 等），继续读流
	zedFrameEnded                           // stream_ended 终态
	zedFrameFailed                          // failed / error 终态
	zedFrameEvent                           // 事件帧（provider 原生事件）
)

// zedClassifyFrame 归一 Zed 帧：对象形态的 status 里只有 failed 是终态，
// 未知形态一律按进行中处理（Zed 新增状态不该把流卡死在等待 stream_ended）。
func zedClassifyFrame(line []byte) (zedFrameClass, map[string]any, string) {
	var env struct {
		Status json.RawMessage `json:"status"`
		Event  json.RawMessage `json:"event"`
		Type   string          `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return zedFrameSkip, nil, ""
	}
	statusKind := ""
	if len(env.Status) > 0 && !bytes.Equal(bytes.TrimSpace(env.Status), []byte("null")) {
		var sv any
		if json.Unmarshal(env.Status, &sv) != nil {
			return zedFrameSkip, nil, ""
		}
		switch v := sv.(type) {
		case string:
			statusKind = v
		case map[string]any:
			if failed, ok := v["failed"].(map[string]any); ok {
				msg := strings.TrimSpace(zedStr(failed["message"]))
				if code := zedStr(failed["code"]); code != "" {
					msg = code + ": " + msg
				}
				if msg == "" {
					msg = "zed 流失败"
				}
				return zedFrameFailed, nil, msg
			}
			statusKind = "in_progress"
		}
	}
	hasEvent := len(env.Event) > 0 && !bytes.Equal(bytes.TrimSpace(env.Event), []byte("null"))
	if statusKind != "" && !hasEvent && env.Type == "" {
		switch statusKind {
		case "stream_ended":
			return zedFrameEnded, nil, ""
		case "error", "failed":
			return zedFrameFailed, nil, "zed 流以 " + statusKind + " 结束"
		}
		return zedFrameInProgress, nil, ""
	}
	var ev map[string]any
	if hasEvent {
		if json.Unmarshal(env.Event, &ev) != nil || ev == nil {
			return zedFrameSkip, nil, ""
		}
	} else if json.Unmarshal(line, &ev) != nil || ev == nil {
		return zedFrameSkip, nil, ""
	}
	return zedFrameEvent, ev, ""
}

// ── 候选处理入口（由代理循环调用） ────────────────────────────────────────

// zedAttempt Zed 候选的处理分支：装配信封 → 懒重铸 → 发请求（凭据被拒时强制
// 重铸重试一次，套餐拒绝时静音该模型）→ 回程按客户端协议翻译。
// 名额释放与失败日志与 wbAttempt 同构。
func (s *Server) zedAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *requestTiming, sm *usageSample) error {
	defer s.store.Release(c.Model.ID)
	cred := zedOf(c.Provider)
	if cred == nil || len(cred.NativeCredential) == 0 {
		err := fmt.Errorf("未登录 Zed 账号（provider 缺少凭证）")
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name, Message: err.Error()})
		return err
	}
	// 懒重铸：临期自动换新，失败不阻断（沿用现有 JWT，上游拒了再强制重铸）
	if cred.tokenExpiring() {
		if nc, err := s.zedRefreshToken(c.Provider, false); err == nil {
			cred = nc
		} else {
			s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Zed JWT 临期重铸失败（沿用现有 token）", Detail: err.Error()})
		}
	}
	model := c.Model.Name
	envelope, plan, err := zedBuildEnvelope(sendBody, responsesMode, model)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindBadRequest, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "构造 Zed 出站请求失败", Detail: err.Error()})
		return err
	}
	wantStream := wbBodyStream(sendBody)
	includeUsage := !wantStream || zedBodyIncludeUsage(sendBody)
	cli, err := s.upstreamClient(c.Provider, 0)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "上游代理配置非法", Detail: err.Error()})
		return err
	}
	url := strings.TrimRight(c.Provider.BaseURL, "/")
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(envelope))
		if err != nil {
			return nil, err
		}
		req.Header = zedCompletionsHeader(cred)
		return cli.Do(req)
	}
	// 发请求并分类。凭据级拒绝（401 / 非 plan 的 403）强制重铸后原候选重试一次；
	// 套餐拒绝（403 含 plan）只静音当前模型，渠道内其他模型继续可用。
	var resp *http.Response
	refreshed := false
	for {
		resp, err = do()
		if err != nil {
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "连接上游失败（网络错误或超时）", Detail: err.Error()})
			return err
		}
		if resp.StatusCode < 400 {
			break
		}
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if zedCredentialRejected(resp.StatusCode, eb) && !refreshed {
			nc, rerr := s.zedRefreshToken(c.Provider, true)
			if rerr != nil {
				s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "Zed 凭据被拒且重铸失败，请重新登录", Detail: rerr.Error()})
				return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, errSnippet(eb))
			}
			s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Zed 凭据被拒，已自动重铸 JWT 并重试该候选"})
			cred = nc
			refreshed = true
			continue
		}
		if zedModelPlanRejected(resp.StatusCode, eb) {
			until, _ := s.store.MuteModel(c.Model.ID, zedPlanMute, "Zed 套餐不含该模型（403）")
			s.logFail(LogEntry{Level: "warn", Kind: kindMuted, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: fmt.Sprintf("Zed 套餐不含该模型，已静音至 %s", until.Local().Format("01-02 15:04")),
				Detail:  errSnippet(eb)})
			return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, errSnippet(eb))
		}
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("Zed 上游返回 HTTP %d", resp.StatusCode), Detail: errSnippet(eb)})
		return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, errSnippet(eb))
	}
	defer resp.Body.Close()
	// 首字节在翻译之前打点：上游发出的第一个 SSE 分片即算响应到达
	upBody := &firstByteReader{r: resp.Body, t: tm}
	usage, wrote, rerr := s.zedRelay(upBody, w, plan, model, wantStream, responsesMode, includeUsage)
	if sm != nil {
		*sm = tm.sample(usage, wantStream)
	}
	if rerr != nil {
		if wrote {
			// 响应头与部分内容已发出，无法换候选；记一条日志便于排查截断
			s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Zed 流式响应中断（已渲染部分内容）", Detail: rerr.Error()})
			return nil
		}
		return rerr
	}
	return nil
}

// zedBodyIncludeUsage chat 客户端是否显式要求 usage 帧（stream_options.include_usage）
func zedBodyIncludeUsage(body []byte) bool {
	var t struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	json.Unmarshal(body, &t)
	return t.StreamOptions.IncludeUsage
}

// zedRelay 把 Zed 上游 SSE 流翻译给客户端。返回（记账用量, 是否已向客户端写字节, 错误）。
// 已写字节后出错只能收尾（调用方记 warn 不换候选）；未写字节时出错交还调用方换候选。
// 输出矩阵：
//
//	provider\客户端 | chat 流式        | chat 非流式          | responses 流式            | responses 非流式
//	gpt（Responses）| 事件→chat 分片   | 事件→chat 分片→聚合  | 事件重新帧化透传          | 收集 response.completed
//	claude（Anth.） | 事件→chat 分片   | 事件→chat 分片→聚合 | 事件→chat 分片→wbRespStream| 分片→聚合→response 对象
func (s *Server) zedRelay(r io.Reader, w http.ResponseWriter, plan *zedWirePlan, model string, wantStream, responsesMode, includeUsage bool) (Usage, bool, error) {
	sc := bufio.NewReader(r)
	fw := flushWriter{w}
	var (
		agg       *wbAgg                // chat 非流式聚合器
		rs        *wbRespStream         // claude → responses 流式渲染器
		fromResp  *zedChatFromResponses // gpt 事件 → chat 分片
		fromAnth  *zedChatFromAnthropic // claude 事件 → chat 分片
		finalResp map[string]any        // gpt + responses 非流式：response.completed 的对象
		streamUse Usage                 // gpt + responses 流式：从 completed 事件嗅探
		started   bool
	)
	switch {
	case plan.provider == zedProviderAnthropic && responsesMode && wantStream:
		rs = newWBRespStream(w, model)
		fromAnth = newZedChatFromAnthropic(rs, model, true)
	case plan.provider == zedProviderAnthropic && wantStream:
		fromAnth = newZedChatFromAnthropic(fw, model, includeUsage)
	case plan.provider == zedProviderAnthropic:
		agg = &wbAgg{}
		fromAnth = newZedChatFromAnthropic(agg, model, true)
	case responsesMode && wantStream:
		// gpt + responses 流式：直接重帧
	case responsesMode:
		// gpt + responses 非流式：收集最终 response 对象
	case wantStream:
		fromResp = newZedChatFromResponses(fw, model, includeUsage)
	default:
		agg = &wbAgg{}
		fromResp = newZedChatFromResponses(agg, model, true)
	}
	// start 首个事件前才写响应头：之前的排队/失败帧阶段仍可换候选
	start := func() {
		if started || !wantStream {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if rs != nil {
			rs.begin()
		}
	}
	// finish 正常收尾：写终片/聚合结果，返回记账用量
	finish := func() (Usage, error) {
		switch {
		case fromAnth != nil && rs != nil: // claude → responses 流式
			_, terr := fromAnth.finish()
			return rs.finish(terr), nil
		case fromAnth != nil && agg != nil: // claude 非流式
			if _, terr := fromAnth.finish(); terr != nil {
				return Usage{}, terr
			}
			out, u := agg.result(model)
			if responsesMode {
				out, u = agg.responsesResult(model)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
			return u, nil
		case fromAnth != nil: // claude → chat 流式
			return fromAnth.finish()
		case fromResp != nil && agg != nil: // gpt 非流式
			if _, terr := fromResp.finish(); terr != nil {
				return Usage{}, terr
			}
			out, u := agg.result(model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
			return u, nil
		case fromResp != nil: // gpt → chat 流式
			return fromResp.finish()
		case wantStream: // gpt → responses 流式（终事件已透传）
			return streamUse, nil
		default: // gpt → responses 非流式
			if finalResp == nil {
				return Usage{}, errors.New("Zed 流结束但缺少 response.completed 事件")
			}
			b, err := json.Marshal(finalResp)
			if err != nil {
				return Usage{}, err
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(b)
			return zedUsageOfResponses(finalResp["usage"]), nil
		}
	}
	for {
		line, readErr := sc.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] != ':' { // SSE 注释行跳过
			if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				line = bytes.TrimSpace(after)
			}
			class, ev, failMsg := zedClassifyFrame(line)
			switch class {
			case zedFrameEvent:
				start()
				switch {
				case fromAnth != nil:
					if err := fromAnth.event(ev); err != nil {
						return Usage{}, started, err
					}
				case fromResp != nil:
					if err := fromResp.event(ev); err != nil {
						return Usage{}, started, err
					}
				default: // gpt + responses：流式重新帧化透传 / 非流式收集最终对象
					evType := zedStr(ev["type"])
					if evType == "" {
						return Usage{}, started, errors.New("Zed 事件缺少 type 字段")
					}
					if wantStream {
						b, err := json.Marshal(ev)
						if err != nil {
							return Usage{}, started, err
						}
						started = true
						if _, err := fmt.Fprintf(fw, "event: %s\ndata: %s\n\n", evType, b); err != nil {
							return Usage{}, started, err
						}
					}
					switch evType {
					case "response.completed", "response.incomplete":
						resp := zedMap(ev["response"])
						if !wantStream {
							finalResp = resp
						} else {
							streamUse = zedUsageOfResponses(resp["usage"])
						}
					}
				}
			case zedFrameEnded:
				u, ferr := finish()
				return u, started, ferr
			case zedFrameFailed:
				if started {
					// 已写出部分内容：尽量收尾（渲染失败终态）后按中断处理
					u, _ := finish()
					return u, true, errors.New(failMsg)
				}
				return Usage{}, false, errors.New(failMsg)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// 上游直接关流（无 stream_ended）：按正常结束收尾
				u, ferr := finish()
				return u, started, ferr
			}
			if started {
				u, _ := finish()
				return u, true, fmt.Errorf("读取 Zed 上游流失败: %v", readErr)
			}
			return Usage{}, false, fmt.Errorf("读取 Zed 上游流失败: %v", readErr)
		}
	}
}
