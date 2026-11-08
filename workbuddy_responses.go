package main

// ─────────────────── /v1/responses ↔ chat/completions 协议转换 ───────────────────
//
// WorkBuddy 上游只有 /v2/chat/completions（/v2/responses 实测 404）。客户端用
// Responses API（Codex 等）时，出站先翻成 chat 形态发上游；回程再把 chat 分片
// 渲染回 responses 事件流（流式）或聚合为 response 对象（非流式）。事件集对齐
// OpenAI Responses 规范的 Codex 兼容子集：created / output_item.* / output_text.* /
// reasoning_summary_* / function_call_arguments.* / completed / incomplete / failed。
//
// 网关不做有状态转换（store / previous_response_id 一律丢弃）：Codex 默认
// store:false 且每轮带全量 input，无状态语义对这类客户端是等价的。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── 入站：responses 请求 → chat 出站体 ───────────────────────────────────

// wbResponsesToChat 把 /v1/responses 请求体翻译成上游认识的 chat/completions 形态。
// 认识的字段显式搬运，其余丢弃；input 为空或全不可用时整体失败。
func wbResponsesToChat(raw []byte) (map[string]any, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, fmt.Errorf("/responses 请求体不是合法 JSON 对象")
	}
	out := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		out["reasoning"] = r // wbClientEffort 读 reasoning.effort，收敛由 wbReasoningNormalize 完成
	}
	var msgs []any
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instr})
	}
	switch in := body["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": in})
	case []any:
		msgs = append(msgs, wbRespItemsToMessages(in)...)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("/responses 请求缺少可用的 input")
	}
	out["messages"] = msgs
	if tools, ok := wbRespToolsToChat(body["tools"]); ok {
		out["tools"] = tools
		if tc, ok := body["tool_choice"]; ok {
			out["tool_choice"] = tc // wbNormalizeToolChoice 兼容字符串与对象两种形态
		}
	}
	if f, ok := body["max_output_tokens"].(float64); ok && f > 0 {
		out["max_tokens"] = int64(f)
	}
	return out, nil
}

// wbRespItemsToMessages responses input 数组 → chat messages。
// 连续的 function_call 聚成一条 assistant.tool_calls（对齐 chat 的并行调用形态）；
// reasoning / item_reference / 未知类型丢弃。
func wbRespItemsToMessages(items []any) []any {
	var msgs []any
	var pending []any // 待聚合进下一条 assistant 消息的 tool_calls
	flush := func() {
		if len(pending) == 0 {
			return
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": pending})
		pending = nil
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := item["type"].(string); t {
		case "message":
			flush()
			role, _ := item["role"].(string)
			content := wbRespContentToChat(item["content"])
			if content == nil {
				continue // 空内容部件（如仅 reasoning 摘要）不产生空消息
			}
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		case "function_call":
			fn := map[string]any{}
			if n, _ := item["name"].(string); n != "" {
				fn["name"] = n
			}
			if a, _ := item["arguments"].(string); a != "" {
				fn["arguments"] = a
			}
			call := map[string]any{"type": "function", "function": fn}
			if id, _ := item["call_id"].(string); id != "" {
				call["id"] = id
			}
			pending = append(pending, call)
		case "function_call_output":
			flush()
			callID, _ := item["call_id"].(string)
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": callID, "content": wbRespOutputText(item["output"]),
			})
		}
	}
	flush()
	return msgs
}

// wbRespContentToChat responses 的 content（字符串或部件数组）→ chat 的 content。
// 单一纯文本部件简化为字符串（兼容只认字符串形态的上游），其余保留部件数组。
func wbRespContentToChat(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []any
		for _, raw := range c {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := p["type"].(string); t {
			case "input_text", "output_text", "text", "summary_text":
				if s, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			case "refusal":
				if s, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			case "input_image":
				if iu := p["image_url"]; iu != nil {
					// 字符串形态由 wbNormalizeImageURL 统一包成 {"url":…}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": iu})
				}
			}
		}
		if len(parts) == 0 {
			return nil
		}
		if len(parts) == 1 {
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
				return p["text"]
			}
		}
		return parts
	}
	return nil
}

// wbRespToolsToChat responses tools（扁平 function 定义）→ chat tools（function 嵌套）。
// 非 function 类型（web_search 等内置工具）上游不支持，丢弃；全部丢弃时返回 false。
func wbRespToolsToChat(v any) ([]any, bool) {
	ts, ok := v.([]any)
	if !ok {
		return nil, false
	}
	var tools []any
	for _, raw := range ts {
		tm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := tm["type"].(string); t != "function" && t != "" {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := tm["description"].(string); ok {
			fn["description"] = d
		}
		if p, ok := tm["parameters"]; ok {
			fn["parameters"] = p
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	return tools, len(tools) > 0
}

// wbRespOutputText responses 的 function_call_output.output（字符串、部件数组或
// {content:[…]} 包装）→ chat tool 消息的 content 字符串
func wbRespOutputText(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case []any:
		var b strings.Builder
		for _, raw := range o {
			if p, ok := raw.(map[string]any); ok {
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	case map[string]any:
		return wbRespOutputText(o["content"])
	}
	return ""
}

// ── 回程：chat 分片 → responses SSE 事件（流式） ─────────────────────────

// wbRespToolItem 一个 function_call item 的累积状态（chat 分片按 index 分流）
type wbRespToolItem struct {
	idx    int // output_index
	itemID string
	callID string
	name   string
	args   strings.Builder
}

// wbRespStream 把上游 chat.completion.chunk 流增量渲染为 Responses SSE 事件。
// 段顺序按实际到达顺序：reasoning（reasoning_content）→ message（content）→
// function_call（tool_calls，chat 分片 index 各成一个 item）；每段惰性开启，
// 下一段出现或收尾时关闭。output_index 与最终 output 数组按关闭顺序保持一致。
type wbRespStream struct {
	w            flushWriter
	model        string
	respID       string
	created      int64
	nextIdx      int // 下一个 output_index
	done         []any
	finishReason string
	usageRaw     json.RawMessage
	lines        []byte

	rsOpen bool
	rsID   string
	rsIdx  int
	rsText strings.Builder

	msgOpen bool
	msgID   string
	msgIdx  int
	msgText strings.Builder

	tools map[int]*wbRespToolItem
}

func newWBRespStream(w http.ResponseWriter, model string) *wbRespStream {
	return &wbRespStream{
		w: flushWriter{w}, model: model,
		respID: "resp_" + wbMessageID(), created: time.Now().Unix(),
		tools: map[int]*wbRespToolItem{},
	}
}

// begin 发出首事件（在写出响应头之后调用一次）
func (s *wbRespStream) begin() {
	s.emit("response.created", map[string]any{"response": s.envelope("in_progress")})
}

// Write 消费过滤后的 SSE 字节流（按行切出 data: 载荷；注释行原样透传——
// 思考持有期间的保活注释不产生事件，只维持客户端连接活跃）
func (s *wbRespStream) Write(p []byte) (int, error) {
	s.lines = append(s.lines, p...)
	for {
		i := bytes.IndexByte(s.lines, '\n')
		if i < 0 {
			break
		}
		line := s.lines[:i]
		s.lines = s.lines[i+1:]
		line = bytes.TrimSuffix(line, []byte("\r"))
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			s.feed(bytes.TrimPrefix(d, []byte(" ")))
		} else if bytes.HasPrefix(line, []byte(":")) {
			fmt.Fprintf(s.w, "%s\n\n", line)
		}
	}
	return len(p), nil
}

// feed 处理一条 chat.completion.chunk，按增量类型分流到各段
func (s *wbRespStream) feed(d []byte) {
	if bytes.Equal(bytes.TrimSpace(d), []byte("[DONE]")) {
		return
	}
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(d, &v) != nil {
		return
	}
	if len(v.Usage) > 0 && string(v.Usage) != "null" {
		s.usageRaw = v.Usage
	}
	if len(v.Choices) == 0 {
		return
	}
	ch := v.Choices[0]
	if ch.Delta.ReasoningContent != "" {
		s.openReasoning()
		s.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": s.rsID, "output_index": s.rsIdx, "summary_index": 0, "delta": ch.Delta.ReasoningContent,
		})
		s.rsText.WriteString(ch.Delta.ReasoningContent)
	}
	if ch.Delta.Content != "" {
		s.closeReasoning()
		s.openMessage()
		s.emit("response.output_text.delta", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIdx, "content_index": 0, "delta": ch.Delta.Content,
		})
		s.msgText.WriteString(ch.Delta.Content)
	}
	for _, tc := range ch.Delta.ToolCalls {
		s.closeReasoning()
		s.closeMessage()
		it := s.tools[tc.Index]
		if it == nil {
			it = &wbRespToolItem{idx: s.nextIdx, itemID: "fc_" + wbMessageID()}
			s.nextIdx++
			s.tools[tc.Index] = it
			s.emit("response.output_item.added", map[string]any{
				"output_index": it.idx,
				"item": map[string]any{"id": it.itemID, "type": "function_call",
					"status": "in_progress", "call_id": "", "name": "", "arguments": ""},
			})
		}
		if tc.ID != "" {
			it.callID = tc.ID
		}
		if tc.Function.Name != "" {
			it.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": it.itemID, "output_index": it.idx, "delta": tc.Function.Arguments,
			})
			it.args.WriteString(tc.Function.Arguments)
		}
	}
	if ch.FinishReason != "" {
		s.finishReason = ch.FinishReason
	}
}

// finish 关闭所有未完段并发终事件（completed / incomplete / failed），返回记账用量
func (s *wbRespStream) finish(cerr error) Usage {
	if cerr != nil {
		s.closeAll()
		resp := s.envelope("failed")
		resp["error"] = map[string]any{"code": "upstream_error", "message": cerr.Error()}
		s.emit("response.failed", map[string]any{"response": resp})
		return Usage{}
	}
	s.closeAll()
	u := wbUsageOf(s.usageRaw)
	resp := s.envelope("completed")
	ev := "response.completed"
	if s.finishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		ev = "response.incomplete"
	}
	resp["output"] = s.done
	resp["usage"] = wbRespUsage(u)
	s.emit(ev, map[string]any{"response": resp})
	return u
}

func (s *wbRespStream) envelope(status string) map[string]any {
	return map[string]any{
		"id": s.respID, "object": "response", "created_at": s.created,
		"status": status, "model": s.model,
	}
}

func (s *wbRespStream) openReasoning() {
	if s.rsOpen {
		return
	}
	s.rsOpen, s.rsID, s.rsIdx = true, "rs_"+wbMessageID(), s.nextIdx
	s.nextIdx++
	s.emit("response.output_item.added", map[string]any{
		"output_index": s.rsIdx,
		"item":         map[string]any{"id": s.rsID, "type": "reasoning", "summary": []any{}},
	})
	s.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id": s.rsID, "output_index": s.rsIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (s *wbRespStream) openMessage() {
	if s.msgOpen {
		return
	}
	s.msgOpen, s.msgID, s.msgIdx = true, "msg_"+wbMessageID(), s.nextIdx
	s.nextIdx++
	s.emit("response.output_item.added", map[string]any{
		"output_index": s.msgIdx,
		"item":         map[string]any{"id": s.msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	s.emit("response.content_part.added", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (s *wbRespStream) closeReasoning() {
	if !s.rsOpen {
		return
	}
	s.rsOpen = false
	text := s.rsText.String()
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": s.rsID, "output_index": s.rsIdx, "summary_index": 0, "text": text,
	})
	s.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": s.rsID, "output_index": s.rsIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	})
	item := map[string]any{"id": s.rsID, "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}}}
	s.emit("response.output_item.done", map[string]any{"output_index": s.rsIdx, "item": item})
	s.done = append(s.done, item)
}

func (s *wbRespStream) closeMessage() {
	if !s.msgOpen {
		return
	}
	s.msgOpen = false
	text := s.msgText.String()
	s.emit("response.output_text.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIdx, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := map[string]any{"id": s.msgID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	s.emit("response.output_item.done", map[string]any{"output_index": s.msgIdx, "item": item})
	s.done = append(s.done, item)
}

func (s *wbRespStream) closeAll() {
	s.closeReasoning()
	s.closeMessage()
	idx := make([]int, 0, len(s.tools))
	for i := range s.tools {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		it := s.tools[i]
		args := it.args.String()
		s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": it.itemID, "output_index": it.idx, "arguments": args,
		})
		item := map[string]any{"id": it.itemID, "type": "function_call", "status": "completed",
			"call_id": wbCallID(it.callID), "name": it.name, "arguments": args}
		s.emit("response.output_item.done", map[string]any{"output_index": it.idx, "item": item})
		s.done = append(s.done, item)
	}
}

// emit 写一条 SSE 事件（event + data 双行），flushWriter 保证即时送达
func (s *wbRespStream) emit(ev string, payload map[string]any) {
	payload["type"] = ev
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev, b)
}

// ── 回程：chat 聚合 → response 对象（非流式） ────────────────────────────

// responsesResult 把 wbAgg 的聚合状态渲染为 response 对象（/v1/responses 非流式）
func (a *wbAgg) responsesResult(model string) ([]byte, Usage) {
	u := wbUsageOf(a.usageRaw)
	resp := map[string]any{
		"id": "resp_" + wbMessageID(), "object": "response",
		"created_at": time.Now().Unix(), "model": model,
		"output": a.respOutputItems(), "usage": wbRespUsage(u),
	}
	if a.finish == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	} else {
		resp["status"] = "completed"
	}
	b, _ := json.Marshal(resp)
	return b, u
}

// respOutputItems 聚合状态 → responses output 数组（reasoning / message / function_call）
func (a *wbAgg) respOutputItems() []any {
	var out []any
	if a.reasoning.Len() > 0 {
		out = append(out, map[string]any{"id": "rs_" + wbMessageID(), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": a.reasoning.String()}}})
	}
	if a.content.Len() > 0 || len(a.tools) == 0 { // 纯文本或全空：保留一条 message
		out = append(out, map[string]any{"id": "msg_" + wbMessageID(), "type": "message",
			"status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": a.content.String(), "annotations": []any{}}}})
	}
	for _, i := range a.sortedToolIdx() {
		t := a.tools[i]
		out = append(out, map[string]any{"id": "fc_" + wbMessageID(), "type": "function_call",
			"status": "completed", "call_id": wbCallID(t.ID), "name": t.FnName, "arguments": t.Args.String()})
	}
	return out
}

// sortedToolIdx tool 累积键（chat 分片 index）升序，保证输出顺序稳定
func (a *wbAgg) sortedToolIdx() []int {
	idx := make([]int, 0, len(a.tools))
	for i := range a.tools {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	return idx
}

// wbCallID chat tool_call 的 id → responses call_id（上游缺 id 时兜底生成）
func wbCallID(id string) string {
	if id != "" {
		return id
	}
	return "call_" + wbMessageID()
}

// wbRespUsage 记账用量 → responses usage 对象
func wbRespUsage(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":         u.Prompt,
		"input_tokens_details": map[string]any{"cached_tokens": u.Cached},
		"output_tokens":        u.Completion,
		"total_tokens":         u.Prompt + u.Completion,
	}
}
