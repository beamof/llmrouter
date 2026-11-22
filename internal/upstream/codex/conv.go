package codex

// ─────────────────── Codex 线协议转换：chat ↔ Responses + SIWC 约束 ───────────────────
//
// SIWC 的 ChatGPT plan usage 对 Responses 请求有一组硬约束（Preview limitations）：
// 恒为 store:false + stream:true；input 必须是条目数组且不能用 system 角色的
// message 条目（改用 instructions / developer）；一票字段（temperature、top_p、
// max_output_tokens、metadata 等）与托管工具（image_generation、file_search、
// code_interpreter、computer_use、MCP、tool_search）不被接受，携带会被 400 拒绝。
// 本文件负责把客户端请求（chat 或 responses 形态）折成合规的出站体，并把上游
// Responses 事件流翻译回 chat 分片（结构与 zed 适配器同构）。

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"llmrouter/internal/wire"
)

// codexUnsupportedFields SIWC 明确不接受的顶层字段，出站前一律剥掉
var codexUnsupportedFields = []string{
	"background", "conversation", "previous_response_id",
	"max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent",
	"prompt", "prompt_cache_retention", "safety_identifier", "service_tier",
	"temperature", "top_logprobs", "top_p", "truncation", "user",
	// chat 侧才有、Responses 不认识的字段也顺手清掉
	"frequency_penalty", "presence_penalty", "logprobs", "n", "stop", "stream_options",
}

// codexUnsupportedTools 不被接受的托管工具类型（保留 function / custom）
func codexToolSupported(t string) bool {
	return t == "function" || t == "custom"
}

// ── 出站归一化 ────────────────────────────────────────────────────────────

// codexNormalizeResponses 把 Responses 形态的请求体折成 SIWC 合规形态：
// store/stream 固定、剥不支持字段、input 归一为条目数组、system 条目折算成
// developer、密文与不可回放的推理条目剥除、托管工具过滤。
func codexNormalizeResponses(body map[string]any) {
	body["stream"] = true
	body["store"] = false
	for _, k := range codexUnsupportedFields {
		delete(body, k)
	}
	switch in := body["input"].(type) {
	case string:
		body["input"] = []any{codexMessageItem("user", in)}
	case []any:
		body["input"] = codexNormalizeItems(in)
	case nil:
		delete(body, "input") // 纯 instructions 的请求（如让模型只输出固定文本）
	}
	if inc := codexSlice(body["include"]); len(inc) > 0 {
		kept := make([]any, 0, len(inc))
		for _, v := range inc {
			// store:false 拿不到加密推理内容，请求它没有意义还会被拒
			if s, ok := v.(string); ok && strings.HasPrefix(s, "reasoning.encrypted_content") {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(body, "include")
		} else {
			body["include"] = kept
		}
	} else if body["include"] == nil {
		delete(body, "include")
	}
	if tools := codexSlice(body["tools"]); len(tools) > 0 {
		kept := make([]any, 0, len(tools))
		for _, raw := range tools {
			t := codexMap(raw)
			if t == nil {
				continue
			}
			if typ := codexStr(t["type"]); !codexToolSupported(typ) {
				continue // image_generation / file_search / code_interpreter / MCP / tool_search…
			}
			kept = append(kept, t)
		}
		if len(kept) == 0 {
			delete(body, "tools")
			delete(body, "tool_choice") // 没有工具时保留选择没有意义，还可能被拒
		} else {
			body["tools"] = kept
		}
	} else if body["tools"] == nil {
		delete(body, "tools")
	}
	codexStripEncrypted(body)
}

// codexMessageItem 构造一个 message 形态的 input 条目
func codexMessageItem(role, text string) map[string]any {
	return map[string]any{"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}}}
}

// codexNormalizeItems input 条目数组归一：删 null 字段、丢 compaction 与无摘要
// reasoning、system 折算成 developer、assistant 文本部件用 output_text。
func codexNormalizeItems(items []any) []any {
	out := make([]any, 0, len(items))
	for _, raw := range items {
		item := codexMap(raw)
		if item == nil {
			continue
		}
		for k, v := range item { // 删 null 值字段（服务端对 null 敏感）
			if v == nil {
				delete(item, k)
			}
		}
		switch codexStr(item["type"]) {
		case "compaction":
			continue // store:false 的无状态转发用不上压缩条目
		case "reasoning":
			if !codexReasoningHasSummary(item) {
				continue // 无摘要的推理条目不可回放
			}
		case "message":
			role := codexStr(item["role"])
			if role == "system" {
				item["role"] = "developer" // SIWC 拒绝 system 条目，语义等价折算
				role = "developer"
			}
			codexNormalizeItemContent(item, role)
			if _, ok := item["content"]; !ok {
				continue // 归一后没有内容的空条目（如只带 null 字段），丢弃
			}
		}
		out = append(out, item)
	}
	return out
}

// codexNormalizeItemContent 条目 content 归一：字符串转类型化部件数组；
// assistant 的纯文本用 output_text，其余 input_text。
func codexNormalizeItemContent(item map[string]any, role string) {
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
			pm := codexMap(p)
			if pm != nil && codexStr(pm["type"]) == "encrypted_content" {
				// 密文部件降级为普通文本部件（store:false 下本来就是拿不到的密文）
				if enc := codexStr(pm["encrypted_content"]); enc != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": enc})
					continue
				}
			}
			parts = append(parts, p)
		}
		item["content"] = parts
	}
}

func codexReasoningHasSummary(item map[string]any) bool {
	for _, raw := range codexSlice(item["summary"]) {
		if p := codexMap(raw); p != nil && strings.TrimSpace(codexStr(p["text"])) != "" {
			return true
		}
	}
	return false
}

// codexStripEncrypted 递归删除所有 encrypted_content 字段
func codexStripEncrypted(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "encrypted_content")
		for _, vv := range t {
			codexStripEncrypted(vv)
		}
	case []any:
		for _, vv := range t {
			codexStripEncrypted(vv)
		}
	}
}

// ── 请求侧：chat → Responses ─────────────────────────────────────────────

// codexChatToResponses chat/completions 请求体 → Responses 请求体。
// system/developer 消息聚合为 instructions，tool 消息转 function_call_output，
// assistant 的 tool_calls 拆成 function_call 条目；reasoning_effort 映射到
// reasoning.effort。SIWC 不支持的字段（temperature 等）不搬运，由归一化兜底。
func codexChatToResponses(body map[string]any, model string) (map[string]any, error) {
	msgs := codexSlice(body["messages"])
	if len(msgs) == 0 {
		return nil, fmt.Errorf("请求缺少 messages")
	}
	out := map[string]any{"model": model}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	var instructions []string
	var items []any
	for _, raw := range msgs {
		m := codexMap(raw)
		if m == nil {
			continue
		}
		switch codexStr(m["role"]) {
		case "system", "developer":
			if t := codexTextContent(m["content"]); t != "" {
				instructions = append(instructions, t)
			}
		case "user":
			if parts := codexChatContentToParts(m["content"], "input_text"); len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": parts})
			}
		case "assistant":
			if t := codexTextContent(m["content"]); t != "" {
				items = append(items, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": t}}})
			}
			for _, rawCall := range codexSlice(m["tool_calls"]) {
				call := codexMap(rawCall)
				fn := codexMap(call["function"])
				if fn == nil {
					continue
				}
				item := map[string]any{"type": "function_call", "name": codexStr(fn["name"]),
					"arguments": codexStr(fn["arguments"])}
				if id := codexStr(call["id"]); id != "" {
					item["call_id"] = id
				}
				items = append(items, item)
			}
		case "tool":
			items = append(items, map[string]any{"type": "function_call_output",
				"call_id": codexStr(m["tool_call_id"]), "output": codexTextContent(m["content"])})
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("请求缺少可转换的 messages")
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	out["input"] = items
	if tools := codexChatToolsToResponses(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if eff := codexStr(body["reasoning_effort"]); eff != "" {
		out["reasoning"] = map[string]any{"effort": eff}
	}
	return out, nil
}

// codexChatContentToParts chat content（字符串或部件数组）→ responses 内容部件
func codexChatContentToParts(v any, textType string) []any {
	var parts []any
	switch c := v.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": textType, "text": c})
		}
	case []any:
		for _, raw := range c {
			p := codexMap(raw)
			if p == nil {
				continue
			}
			switch codexStr(p["type"]) {
			case "text":
				if t := codexStr(p["text"]); t != "" {
					parts = append(parts, map[string]any{"type": textType, "text": t})
				}
			case "image_url":
				if iu := codexMap(p["image_url"]); iu != nil {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": codexStr(iu["url"])})
				}
			}
		}
	}
	return parts
}

// codexChatToolsToResponses chat tools（function 嵌套）→ responses tools（扁平）
func codexChatToolsToResponses(v any) []any {
	var out []any
	for _, raw := range codexSlice(v) {
		t := codexMap(raw)
		if t == nil {
			continue
		}
		typ := codexStr(t["type"])
		if !codexToolSupported(typ) {
			continue
		}
		if typ == "function" {
			fn := codexMap(t["function"])
			if fn == nil || codexStr(fn["name"]) == "" {
				continue
			}
			tool := map[string]any{"type": "function", "name": codexStr(fn["name"])}
			if d := codexStr(fn["description"]); d != "" {
				tool["description"] = d
			}
			if p, ok := fn["parameters"]; ok {
				tool["parameters"] = p
			}
			out = append(out, tool)
			continue
		}
		// custom 工具已是 responses 形态（name + description + format），直通
		if codexStr(t["name"]) != "" {
			out = append(out, t)
		}
	}
	return out
}

// ── 回程侧：Responses 事件 → chat 分片 ───────────────────────────────────

// codexChunkSSE 生成并发送一条 chat.completion.chunk SSE 帧
func codexChunkSSE(w io.Writer, id string, created int64, model string, delta map[string]any, finish string, usage any) error {
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

// codexUsageChunk 记账用量 → chat 形态 usage 对象（wire.Aggregator 认这个形状）
func codexUsageChunk(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.Prompt,
		"completion_tokens": u.Completion,
		"total_tokens":      u.Prompt + u.Completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.Cached,
		},
	}
}

// codexChatFromResponses 消费 Responses 事件（map 形态），渲染为 chat 分片流写入 w。
// 事件集是 codexChatToResponses 的逆映射 + Codex 兼容子集（output_text/reasoning/
// function_call 系列）。finish() 补终片与 [DONE]，返回记账用量。
type codexChatFromResponses struct {
	w            io.Writer
	model        string
	id           string
	created      int64
	started      bool           // 已发首片（含 role）
	finishReason string         // 终片 finish_reason
	usage        Usage          // 从 completed 事件的 response.usage 提取
	err          error          // response.failed 时的上游错误
	emitUsage    bool           // 结束时是否补发 usage 帧（wire.Aggregator 需要）
	tools        map[string]int // item_id → tool_call index
	nextTool     int
}

func newCodexChatFromResponses(w io.Writer, model string, emitUsage bool) *codexChatFromResponses {
	return &codexChatFromResponses{
		w: w, model: model, id: "chatcmpl-" + wire.MessageID(), created: time.Now().Unix(),
		emitUsage: emitUsage, tools: map[string]int{},
	}
}

// start 确保首片（role）已发出
func (s *codexChatFromResponses) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return codexChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"role": "assistant", "content": ""}, "", nil)
}

// event 处理一条 Responses 事件对象
func (s *codexChatFromResponses) event(ev map[string]any) error {
	switch codexStr(ev["type"]) {
	case "response.output_item.added":
		item := codexMap(ev["item"])
		if item == nil {
			return nil
		}
		switch codexStr(item["type"]) {
		case "message", "reasoning":
			return s.start()
		case "function_call":
			if err := s.start(); err != nil {
				return err
			}
			idx := s.nextTool
			s.nextTool++
			s.tools[codexStr(item["id"])] = idx
			call := map[string]any{"index": idx, "type": "function",
				"function": map[string]any{"name": codexStr(item["name"]), "arguments": ""}}
			if id := codexStr(item["call_id"]); id != "" {
				call["id"] = id
			}
			return codexChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"tool_calls": []any{call}}, "", nil)
		}
	case "response.output_text.delta":
		if err := s.start(); err != nil {
			return err
		}
		return codexChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"content": codexStr(ev["delta"])}, "", nil)
	case "response.reasoning_summary_text.delta":
		if err := s.start(); err != nil {
			return err
		}
		return codexChunkSSE(s.w, s.id, s.created, s.model, map[string]any{"reasoning_content": codexStr(ev["delta"])}, "", nil)
	case "response.function_call_arguments.delta":
		if err := s.start(); err != nil {
			return err
		}
		idx := s.tools[codexStr(ev["item_id"])]
		return codexChunkSSE(s.w, s.id, s.created, s.model,
			map[string]any{"tool_calls": []any{map[string]any{"index": idx,
				"function": map[string]any{"arguments": codexStr(ev["delta"])}}}}, "", nil)
	case "response.completed", "response.incomplete":
		resp := codexMap(ev["response"])
		s.usage = codexUsageOfResponses(resp["usage"])
		s.finishReason = "stop"
		if len(s.tools) > 0 {
			s.finishReason = "tool_calls"
		}
		if codexStr(ev["type"]) == "response.incomplete" {
			s.finishReason = "length"
		}
	case "response.failed":
		resp := codexMap(ev["response"])
		msg := "上游返回错误"
		if e := codexMap(resp["error"]); e != nil {
			if m := codexStr(e["message"]); m != "" {
				msg = m
			}
			if c := codexStr(e["code"]); c != "" {
				msg = c + ": " + msg
			}
		}
		s.err = fmt.Errorf("%s", msg)
		s.finishReason = "stop"
	}
	return nil
}

// finish 补终片（finish_reason）、可选 usage 帧与 [DONE]，返回记账用量
func (s *codexChatFromResponses) finish() (Usage, error) {
	if err := s.start(); err != nil {
		return Usage{}, err
	}
	finish := s.finishReason
	if finish == "" {
		finish = "stop"
	}
	if err := codexChunkSSE(s.w, s.id, s.created, s.model, map[string]any{}, finish, nil); err != nil {
		return Usage{}, err
	}
	if s.emitUsage {
		if err := codexChunkSSE(s.w, s.id, s.created, s.model, nil, "", codexUsageChunk(s.usage)); err != nil {
			return Usage{}, err
		}
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	return s.usage, s.err // 交给上层决定能否换候选
}

// codexUsageOfResponses responses 事件里的 response.usage → 记账用量
func codexUsageOfResponses(v any) Usage {
	u := codexMap(v)
	if u == nil {
		return Usage{}
	}
	p, _ := codexInt(u["input_tokens"])
	c, _ := codexInt(u["output_tokens"])
	cached, _ := codexInt(codexMap(u["input_tokens_details"])["cached_tokens"])
	return Usage{Prompt: int64(p), Cached: int64(cached), Completion: int64(c)}
}

// ── 取值小工具（map[string]any 反序列化形态） ─────────────────────────────

func codexStr(v any) string {
	s, _ := v.(string)
	return s
}

func codexMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func codexSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func codexInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// codexTextContent chat 消息 content → 纯文本（多部件拼接）
func codexTextContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			if p, ok := raw.(map[string]any); ok {
				if t := codexStr(p["text"]); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}
