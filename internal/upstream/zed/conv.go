package zed

// ─────────────────── Zed 线协议转换器（chat 中间形态 ↔ 上游原生形态） ───────────────────
//
// 本网关的内部中间形态是 chat/completions（与 WorkBuddy 路径一致，已有的
// wbResponsesToChat / wire.ResponsesStream / wire.Aggregator 都围绕它）。Zed 上游的两类模型各有
// 原生线协议：gpt-* 走 OpenAI Responses，claude-* 走 Anthropic Messages。
// 请求侧：chat → responses / chat → anthropic；回程侧：上游事件 → chat 分片，
// 再按客户端协议直写（chat 流式）、聚合（chat 非流式）或渲染回 responses 事件
// （复用 wire.ResponsesStream / wire.Aggregator，它们同样消费 chat 分片字节）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/wire"
	"strings"
	"time"
)

// ── 取值小工具（map[string]any 反序列化形态） ──────────────────────────────

func zedStr(v any) string {
	s, _ := v.(string)
	return s
}

func zedMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func zedSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func zedInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// zedTextContent chat 消息 content → 纯文本（多部件拼接）
func zedTextContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			if p, ok := raw.(map[string]any); ok {
				if t := zedStr(p["text"]); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ── 请求侧：chat → OpenAI Responses ───────────────────────────────────────

// zedChatToResponses chat/completions 请求体 → Responses 请求体。
// system/developer 消息聚合为 instructions，tool 消息转 function_call_output，
// assistant 的 tool_calls 拆成 function_call 条目。
func zedChatToResponses(body map[string]any, model string) (map[string]any, error) {
	msgs := zedSlice(body["messages"])
	if len(msgs) == 0 {
		return nil, fmt.Errorf("请求缺少 messages")
	}
	out := map[string]any{"model": model}
	for _, k := range []string{"temperature", "top_p", "tool_choice"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	var instructions []string
	var items []any
	for _, raw := range msgs {
		m := zedMap(raw)
		if m == nil {
			continue
		}
		switch zedStr(m["role"]) {
		case "system", "developer":
			if t := zedTextContent(m["content"]); t != "" {
				instructions = append(instructions, t)
			}
		case "user":
			if parts := zedChatContentToResponsesParts(m["content"], "input_text"); len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": parts})
			}
		case "assistant":
			if t := zedTextContent(m["content"]); t != "" {
				items = append(items, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": t}}})
			}
			for _, rawCall := range zedSlice(m["tool_calls"]) {
				call := zedMap(rawCall)
				fn := zedMap(call["function"])
				if fn == nil {
					continue
				}
				item := map[string]any{"type": "function_call", "name": zedStr(fn["name"]),
					"arguments": zedStr(fn["arguments"])}
				if id := zedStr(call["id"]); id != "" {
					item["call_id"] = id
				}
				items = append(items, item)
			}
		case "tool":
			item := map[string]any{"type": "function_call_output",
				"call_id": zedStr(m["tool_call_id"]), "output": zedTextContent(m["content"])}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("请求缺少可转换的 messages")
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	out["input"] = items
	if tools := zedChatToolsToResponses(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := zedInt(body[k]); ok && n > 0 {
			out["max_output_tokens"] = float64(n) // 存 JSON 归一形态，取值统一走 zedInt
			break
		}
	}
	return out, nil
}

// zedChatContentToResponsesParts chat content（字符串或部件数组）→ responses 内容部件
func zedChatContentToResponsesParts(v any, textType string) []any {
	var parts []any
	switch c := v.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": textType, "text": c})
		}
	case []any:
		for _, raw := range c {
			p := zedMap(raw)
			if p == nil {
				continue
			}
			switch zedStr(p["type"]) {
			case "text":
				if t := zedStr(p["text"]); t != "" {
					parts = append(parts, map[string]any{"type": textType, "text": t})
				}
			case "image_url":
				if iu := zedMap(p["image_url"]); iu != nil {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": zedStr(iu["url"])})
				}
			}
		}
	}
	return parts
}

// zedChatToolsToResponses chat tools（function 嵌套）→ responses tools（扁平）
func zedChatToolsToResponses(v any) []any {
	var out []any
	for _, raw := range zedSlice(v) {
		t := zedMap(raw)
		if t == nil || zedStr(t["type"]) != "function" {
			continue
		}
		fn := zedMap(t["function"])
		if fn == nil || zedStr(fn["name"]) == "" {
			continue
		}
		tool := map[string]any{"type": "function", "name": zedStr(fn["name"])}
		if d := zedStr(fn["description"]); d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			tool["parameters"] = p
		}
		out = append(out, tool)
	}
	return out
}

// ── 请求侧：chat → Anthropic Messages ────────────────────────────────────

// zedAnthropicDefaultMaxTokens Anthropic 线路的默认输出上限。Zed 原生请求与
// Claude Sonnet 4.5 都按 8192 兜底（参照 ccLoad 实测），不要随意调大。
const zedAnthropicDefaultMaxTokens = 8192

// zedChatToAnthropic chat/completions 请求体 → Anthropic Messages 请求体。
// tool 消息转 tool_result 块，assistant 的 tool_calls 转 tool_use 块，
// 图片按 Anthropic 源格式（data URL → base64，http(s) → url）转换。
func zedChatToAnthropic(body map[string]any, model string) (map[string]any, error) {
	msgs := zedSlice(body["messages"])
	if len(msgs) == 0 {
		return nil, fmt.Errorf("请求缺少 messages")
	}
	out := map[string]any{"model": model, "max_tokens": float64(zedAnthropicDefaultMaxTokens)}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := zedInt(body[k]); ok && n > 0 {
			out["max_tokens"] = float64(n) // 存 JSON 归一形态，取值统一走 zedInt
			break
		}
	}
	if stop := zedStr(body["stop"]); stop != "" {
		out["stop_sequences"] = []any{stop}
	}
	var system []any
	var messages []any
	for _, raw := range msgs {
		m := zedMap(raw)
		if m == nil {
			continue
		}
		role := zedStr(m["role"])
		switch role {
		case "system", "developer":
			if t := zedTextContent(m["content"]); t != "" {
				system = append(system, map[string]any{"type": "text", "text": t})
			}
			continue
		case "tool":
			// Anthropic 没有 tool 角色：工具结果作为 user 消息的 tool_result 块
			messages = append(messages, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": zedStr(m["tool_call_id"]),
					"content": zedTextContent(m["content"]), "is_error": false},
			}})
			continue
		}
		anthRole := "user"
		if role == "assistant" {
			anthRole = "assistant"
		}
		var content []any
		if t := zedTextContent(m["content"]); t != "" && zedStr(m["content"]) == t {
			content = append(content, map[string]any{"type": "text", "text": t})
		} else {
			for _, p := range zedAnthropicParts(m["content"]) {
				content = append(content, p)
			}
		}
		for _, rawCall := range zedSlice(m["tool_calls"]) {
			call := zedMap(rawCall)
			fn := zedMap(call["function"])
			if fn == nil || zedStr(fn["name"]) == "" {
				continue
			}
			input := map[string]any{}
			json.Unmarshal([]byte(zedStr(fn["arguments"])), &input)
			content = append(content, map[string]any{"type": "tool_use", "id": zedStr(call["id"]),
				"name": zedStr(fn["name"]), "input": input})
		}
		if len(content) == 0 {
			continue
		}
		messages = append(messages, map[string]any{"role": anthRole, "content": content})
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("请求缺少可转换的 messages")
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = messages
	if tools := zedChatToolsToAnthropic(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := zedToolChoiceToAnthropic(body["tool_choice"]); ok {
			out["tool_choice"] = tc
		}
	}
	if thinking := zedMap(body["thinking"]); thinking != nil {
		out["thinking"] = thinking // Anthropic 形态直通（chat 客户端一般不带，带上则尊重）
	}
	return out, nil
}

// zedAnthropicParts chat 部件数组 → Anthropic 内容块（文本 / 图片）
func zedAnthropicParts(v any) []any {
	var parts []any
	for _, raw := range zedSlice(v) {
		p := zedMap(raw)
		if p == nil {
			continue
		}
		switch zedStr(p["type"]) {
		case "text":
			if t := zedStr(p["text"]); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image_url":
			if iu := zedMap(p["image_url"]); iu != nil {
				if src := zedImageSource(zedStr(iu["url"])); src != nil {
					parts = append(parts, map[string]any{"type": "image", "source": src})
				}
			}
		}
	}
	return parts
}

// zedImageSource 图片 URL → Anthropic 图片源。data: URL 拆出 base64 数据与媒体类型，
// http(s) 用 url 源；其余（空串等）返回 nil 跳过。
func zedImageSource(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		media, data, ok := strings.Cut(rest, ",")
		if !ok {
			return nil
		}
		media = strings.TrimSuffix(media, ";base64")
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil
		}
		return map[string]any{"type": "base64", "media_type": media, "data": data}
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return map[string]any{"type": "url", "url": url}
	}
	return nil
}

// zedChatToolsToAnthropic chat tools → Anthropic 工具定义（input_schema）
func zedChatToolsToAnthropic(v any) []any {
	var out []any
	for _, raw := range zedSlice(v) {
		t := zedMap(raw)
		if t == nil || zedStr(t["type"]) != "function" {
			continue
		}
		fn := zedMap(t["function"])
		if fn == nil || zedStr(fn["name"]) == "" {
			continue
		}
		tool := map[string]any{"name": zedStr(fn["name"])}
		if d := zedStr(fn["description"]); d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			tool["input_schema"] = p
		}
		out = append(out, tool)
	}
	return out
}

// zedToolChoiceToAnthropic chat tool_choice → Anthropic tool_choice。
// "none" 由调用方以丢弃 tools 实现，这里不处理。
func zedToolChoiceToAnthropic(v any) (map[string]any, bool) {
	switch c := v.(type) {
	case string:
		switch c {
		case "auto", "required":
			return map[string]any{"type": map[bool]string{true: "any", false: "auto"}[c == "required"]}, true
		}
	case map[string]any:
		name := zedStr(c["name"])
		if fn := zedMap(c["function"]); fn != nil && name == "" {
			name = zedStr(fn["name"])
		}
		if name != "" {
			return map[string]any{"type": "tool", "name": name}, true
		}
	}
	return nil, false
}

// ── 回程侧：上游事件 → chat 分片（两个翻译器共用骨架） ────────────────────

// zedChunkSSE 生成并发送一条 chat.completion.chunk SSE 帧
func zedChunkSSE(w io.Writer, id string, created int64, model string, delta map[string]any, finish string, usage any) error {
	chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model}
	if usage != nil {
		chunk["usage"] = usage
		chunk["choices"] = []any{}
	} else {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk["choices"] = []any{choice}
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// zedUsageChunk 记账用量 → chat 形态 usage 对象（parseUsageObject / wire.Aggregator 认这个形状）
func zedUsageChunk(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.Prompt,
		"completion_tokens": u.Completion,
		"total_tokens":      u.Prompt + u.Completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.Cached,
		},
	}
}

// zedChatFromResponses 消费 Responses 事件（map 形态），渲染为 chat 分片流写入 w。
// 事件集是 zedChatToResponses 的逆映射 + Codex 兼容子集（output_text/reasoning/
// function_call 系列）。finish() 补终片与 [DONE]，返回记账用量。
type zedChatFromResponses struct {
	w            io.Writer
	model        string
	id           string
	created      int64
	started      bool           // 已发首片（含 role）
	finishReason string         // 终片 finish_reason
	usage        Usage          // 从 completed 事件的 response.usage 提取
	err          error          // response.failed 时的上游错误
	emitUsage    bool           // 结束时是否补发 usage 帧（wire.Aggregator/wire.ResponsesStream 需要）
	tools        map[string]int // item_id → tool_call index
	nextTool     int
}

func newZedChatFromResponses(w io.Writer, model string, emitUsage bool) *zedChatFromResponses {
	return &zedChatFromResponses{
		w: w, model: model, id: "chatcmpl-" + wire.MessageID(), created: time.Now().Unix(),
		emitUsage: emitUsage, tools: map[string]int{},
	}
}

// start 确保首片（role）已发出
func (s *zedChatFromResponses) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"role": "assistant", "content": ""}, "", nil)
}

// event 处理一条 Responses 事件对象
func (s *zedChatFromResponses) event(ev map[string]any) error {
	switch zedStr(ev["type"]) {
	case "response.output_item.added":
		item := zedMap(ev["item"])
		if item == nil {
			return nil
		}
		switch zedStr(item["type"]) {
		case "message", "reasoning":
			return s.start()
		case "function_call":
			if err := s.start(); err != nil {
				return err
			}
			idx := s.nextTool
			s.nextTool++
			s.tools[zedStr(item["id"])] = idx
			call := map[string]any{"index": idx, "type": "function",
				"function": map[string]any{"name": zedStr(item["name"]), "arguments": ""}}
			if id := zedStr(item["call_id"]); id != "" {
				call["id"] = id
			}
			return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"tool_calls": []any{call}}, "", nil)
		}
	case "response.output_text.delta":
		if err := s.start(); err != nil {
			return err
		}
		return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"content": zedStr(ev["delta"])}, "", nil)
	case "response.reasoning_summary_text.delta":
		if err := s.start(); err != nil {
			return err
		}
		return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"reasoning_content": zedStr(ev["delta"])}, "", nil)
	case "response.function_call_arguments.delta":
		if err := s.start(); err != nil {
			return err
		}
		idx := s.tools[zedStr(ev["item_id"])]
		return zedChunkSSE(s.w, s.id, s.created, s.model,
			map[string]any{"tool_calls": []any{map[string]any{"index": idx,
				"function": map[string]any{"arguments": zedStr(ev["delta"])}}}}, "", nil)
	case "response.completed", "response.incomplete":
		resp := zedMap(ev["response"])
		s.usage = zedUsageOfResponses(resp["usage"])
		s.finishReason = "stop"
		if len(s.tools) > 0 {
			s.finishReason = "tool_calls"
		}
		if zedStr(ev["type"]) == "response.incomplete" {
			s.finishReason = "length"
		}
	case "response.failed":
		resp := zedMap(ev["response"])
		msg := "上游返回错误"
		if e := zedMap(resp["error"]); e != nil && zedStr(e["message"]) != "" {
			msg = zedStr(e["message"])
		}
		s.err = fmt.Errorf("%s", msg)
		s.finishReason = "stop"
	}
	return nil
}

// finish 补终片（finish_reason）、可选 usage 帧与 [DONE]，返回记账用量
func (s *zedChatFromResponses) finish() (Usage, error) {
	if err := s.start(); err != nil {
		return Usage{}, err
	}
	finish := s.finishReason
	if finish == "" {
		finish = "stop"
	}
	if err := zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{}, finish, nil); err != nil {
		return Usage{}, err
	}
	if s.emitUsage {
		if err := zedChunkSSE(s.w, s.id, s.created, s.model, nil, "", zedUsageChunk(s.usage)); err != nil {
			return Usage{}, err
		}
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	return s.usage, s.err // 交给上层决定能否换候选
}

// zedUsageOfResponses responses 事件里的 response.usage → 记账用量
func zedUsageOfResponses(v any) Usage {
	u := zedMap(v)
	if u == nil {
		return Usage{}
	}
	p, _ := zedInt(u["input_tokens"])
	c, _ := zedInt(u["output_tokens"])
	cached, _ := zedInt(zedMap(u["input_tokens_details"])["cached_tokens"])
	return Usage{Prompt: int64(p), Cached: int64(cached), Completion: int64(c)}
}

// zedChatFromAnthropic 消费 Anthropic 事件（map 形态），渲染为 chat 分片流写入 w。
// stop_reason 映射：end_turn/stop_sequence→stop、tool_use→tool_calls、max_tokens→length。
// Zed 的 Anthropic 流可能缺 message_start / 收尾事件，finish() 会补齐（参照 ccLoad）。
type zedChatFromAnthropic struct {
	w            io.Writer
	model        string
	id           string
	created      int64
	started      bool
	finishReason string // 终片 finish_reason（message_delta 推导）
	usage        Usage
	err          error
	emitUsage    bool
	stopped      bool
	openBlock    *int           // 未关闭的内容块 index（content_block_stop 收尾用）
	toolUse      bool           // 出现过 tool_use 块（决定终片 finish_reason）
	toolIdx      map[string]int // 内容块 index → tool_call index
	nextTool     int
}

func newZedChatFromAnthropic(w io.Writer, model string, emitUsage bool) *zedChatFromAnthropic {
	return &zedChatFromAnthropic{
		w: w, model: model, id: "chatcmpl-" + wire.MessageID(), created: time.Now().Unix(),
		emitUsage: emitUsage, toolIdx: map[string]int{},
	}
}

func (s *zedChatFromAnthropic) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"role": "assistant", "content": ""}, "", nil)
}

// event 处理一条 Anthropic 事件对象
func (s *zedChatFromAnthropic) event(ev map[string]any) error {
	switch zedStr(ev["type"]) {
	case "message_start":
		// chat 侧在流开始时就要 role 首片（对齐 OpenAI 流形态）
		if err := s.start(); err != nil {
			return err
		}
		msg := zedMap(ev["message"])
		u := zedMap(msg["usage"])
		s.usage.Prompt = int64(zedOr0(u["input_tokens"]))
		s.usage.Cached = int64(zedOr0(u["cache_read_input_tokens"]))
	case "content_block_start":
		if err := s.start(); err != nil {
			return err
		}
		block := zedMap(ev["content_block"])
		idx, _ := zedInt(ev["index"])
		open := idx
		s.openBlock = &open
		if block != nil && zedStr(block["type"]) == "tool_use" {
			s.toolUse = true
			tIdx := s.nextTool
			s.nextTool++
			s.toolIdx[fmt.Sprint(idx)] = tIdx
			call := map[string]any{"index": tIdx, "id": zedStr(block["id"]), "type": "function",
				"function": map[string]any{"name": zedStr(block["name"]), "arguments": ""}}
			return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"tool_calls": []any{call}}, "", nil)
		}
	case "content_block_delta":
		if err := s.start(); err != nil {
			return err
		}
		delta := zedMap(ev["delta"])
		if delta == nil {
			return nil
		}
		switch zedStr(delta["type"]) {
		case "text_delta":
			return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"content": zedStr(delta["text"])}, "", nil)
		case "thinking_delta":
			return zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"reasoning_content": zedStr(delta["thinking"])}, "", nil)
		case "input_json_delta":
			idx, _ := zedInt(ev["index"])
			tIdx := s.toolIdx[fmt.Sprint(idx)]
			return zedChunkSSE(s.w, s.id, s.created, s.model,
				map[string]any{"tool_calls": []any{map[string]any{"index": tIdx,
					"function": map[string]any{"arguments": zedStr(delta["partial_json"])}}}}, "", nil)
		}
	case "content_block_stop":
		s.openBlock = nil
	case "message_delta":
		d := zedMap(ev["delta"])
		switch zedStr(d["stop_reason"]) {
		case "tool_use":
			s.finishReason = "tool_calls"
		case "max_tokens":
			s.finishReason = "length"
		case "end_turn", "stop_sequence", "":
			if zedStr(d["stop_reason"]) != "" {
				s.finishReason = "stop"
			}
		default:
			s.finishReason = "stop"
		}
		if u := zedMap(ev["usage"]); u != nil {
			s.usage.Completion = int64(zedOr0(u["output_tokens"]))
		}
	case "message_stop":
		s.stopped = true
		s.openBlock = nil
	case "error":
		msg := zedStr(zedMap(ev["error"])["message"])
		if msg == "" {
			msg = "上游返回错误事件"
		}
		s.err = fmt.Errorf("%s", msg)
	}
	return nil
}

func zedOr0(v any) int {
	n, _ := zedInt(v)
	return n
}

// finish 补终片、可选 usage 帧与 [DONE]。Zed 的 Anthropic 流不带 message_stop 时
// 按 tool_use 与否推导 finish_reason（end_turn / tool_use → stop / tool_calls）。
func (s *zedChatFromAnthropic) finish() (Usage, error) {
	if err := s.start(); err != nil {
		return Usage{}, err
	}
	finish := s.finishReason
	if finish == "" {
		if s.toolUse {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if err := zedChunkSSE(s.w, s.id, s.created, s.model, map[string]any{}, finish, nil); err != nil {
		return Usage{}, err
	}
	if s.emitUsage {
		if err := zedChunkSSE(s.w, s.id, s.created, s.model, nil, "", zedUsageChunk(s.usage)); err != nil {
			return Usage{}, err
		}
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	return s.usage, s.err
}
