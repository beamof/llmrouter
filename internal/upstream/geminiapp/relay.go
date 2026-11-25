package geminiapp

// 回程交付：把回合产出的正文增量与工具调用翻译成客户端协议。
//
// 流式带「前缀闸门」：首段不足 2048 字节、且还没有工具调用时先不写响应头，
// 上游此时失败仍可换候选或回传真实错误码；提交之后出错只能收尾（由 runTurn
// 决定按 incomplete 平稳结束还是报错）。非流式全程缓冲，结束才写。
// 结构化输出（response_format）在终检通过前不提交。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llmrouter/internal/wire"
)

// incompleteDetails 中断标记（网关兼容扩展字段，忽略它的客户端按正常完成收尾）
type incompleteDetails struct {
	Reason string `json:"reason"`
	Code   string `json:"code"`
}

// chatUsage / responsesUsage 用量是网关估算值（上游不带 token 计数）
type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	Estimated        bool  `json:"estimated"`
}

type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Estimated    bool  `json:"estimated"`
}

// ── chat 流式分片 ──────────────────────────────────────────────────────────

type chatToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function toolCallFunction `json:"function"`
}

type chatDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatChunk struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []chatChoice       `json:"choices"`
	Usage             *chatUsage         `json:"usage,omitempty"`
	Incomplete        bool               `json:"incomplete,omitempty"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details,omitempty"`
}

// ── chat 非流式 ────────────────────────────────────────────────────────────

type chatMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type chatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatUsage             `json:"usage"`
}

// ── responses 输出条目 ──────────────────────────────────────────────────────

type outputTextPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type messageItem struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Status  string           `json:"status"`
	Role    string           `json:"role"`
	Content []outputTextPart `json:"content"`
}

// functionCallItem 命名空间工具输出 namespace + 原始名，不暴露内部别名
type functionCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// newOutputTextPart output_text 部件；annotations / logprobs 恒为空数组而不是 null
func newOutputTextPart(text string) outputTextPart {
	return outputTextPart{Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{}}
}

// respItem responses 流里已开启的输出条目
type respItem struct {
	call *toolCall // nil = message 条目
	id   string
}

func (it respItem) final(status, text string) any {
	if it.call == nil {
		return messageItem{ID: it.id, Type: "message", Status: status, Role: "assistant",
			Content: []outputTextPart{newOutputTextPart(text)}}
	}
	return functionCallItem{ID: it.id, Type: "function_call", Status: status, CallID: it.call.ID,
		Namespace: it.call.nsNamespace, Name: it.call.callName(), Arguments: it.call.Function.Arguments}
}

// callName 对外的工具名：命名空间工具用原始名
func (c *toolCall) callName() string {
	if c.nsName != "" {
		return c.nsName
	}
	return c.Function.Name
}

type heldItem struct {
	content string
	call    *toolCall
}

// delivery 回程交付层
type delivery struct {
	w             http.ResponseWriter
	fw            wire.FlushWriter
	model         string
	responsesMode bool
	stream        bool
	includeUsage  bool
	holdAll       bool // 结构化输出：终检通过前不提交
	id            string
	created       int64
	req           *canonicalRequest

	started     bool
	wireBytes   int // 已写给客户端的正文字节数
	held        []heldItem
	heldBytes   int
	outputBytes int // 用量估算口径：正文 + 工具调用 JSON 的字节数
	text        strings.Builder
	calls       []toolCall

	seq   int
	items []respItem // responses 流式：按开启顺序
}

func newDelivery(w http.ResponseWriter, plan *canonicalRequest, responsesMode, stream, includeUsage bool) *delivery {
	d := &delivery{
		w: w, model: plan.model, responsesMode: responsesMode, stream: stream,
		includeUsage: includeUsage, holdAll: stream && plan.responseFormat != nil,
		created: time.Now().Unix(), req: plan,
	}
	d.fw = wire.FlushWriter{W: w}
	if responsesMode {
		d.id = "resp_" + randomHex(16)
	} else {
		d.id = "chatcmpl-" + randomHex(16)
	}
	return d
}

func (d *delivery) usage() Usage { return d.req.estimateUsage(d.outputBytes) }
func (d *delivery) chatUsage() *chatUsage {
	u := d.usage()
	return &chatUsage{PromptTokens: u.Prompt, CompletionTokens: u.Completion, TotalTokens: u.Prompt + u.Completion, Estimated: true}
}
func (d *delivery) responsesUsage() *responsesUsage {
	u := d.usage()
	return &responsesUsage{InputTokens: u.Prompt, OutputTokens: u.Completion, TotalTokens: u.Prompt + u.Completion, Estimated: true}
}

// content 正文增量
func (d *delivery) content(delta string) error {
	d.outputBytes += len(delta)
	if !d.stream {
		d.text.WriteString(delta)
		return nil
	}
	// 已提交后不再需要前缀闸门：后续增量直接下发，避免二次攒批造成流式延迟。
	if d.started && !d.holdAll {
		return d.writeContent(delta)
	}
	d.held = append(d.held, heldItem{content: delta})
	d.heldBytes += len(delta)
	if !d.holdAll && d.heldBytes >= prefixBytes {
		return d.commit()
	}
	return nil
}

// call 工具调用：已确定的输出，连同之前扣住的正文一起提交
func (d *delivery) call(tc toolCall) error {
	d.outputBytes += len(mustCompactJSON(tc))
	if !d.stream {
		d.calls = append(d.calls, tc)
		return nil
	}
	d.held = append(d.held, heldItem{call: &tc})
	return d.commit()
}

func (d *delivery) committed() (int, int) {
	if !d.stream || !d.started {
		return 0, 0
	}
	return d.wireBytes, len(d.calls)
}

// reset 丢弃尚未提交的全部产出。只在响应头还没写给客户端时调用：上游改写了尚未
// 提交的快照，或结构化输出终检后整体重发。
func (d *delivery) reset() {
	d.held, d.heldBytes, d.outputBytes, d.wireBytes = nil, 0, 0, 0
	d.text.Reset()
	d.calls, d.items = nil, nil
}

// keepalive 流式已提交后的静默保活（SSE 注释行，不产生事件）
func (d *delivery) keepalive() {
	if d.started {
		fmt.Fprint(&d.fw, ": keepalive\n\n")
	}
}

// commit 写出响应头与首事件（仅首次），并冲刷已扣住的输出。
// 已提交后再次调用只冲刷新扣住的增量，不能直接返回：流式响应的后续正文
// 也要逐段下发。
func (d *delivery) commit() error {
	if !d.started {
		d.started = true
		d.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		d.w.Header().Set("Cache-Control", "no-cache")
		d.w.Header().Set("X-Accel-Buffering", "no")
		d.w.WriteHeader(http.StatusOK)
		if d.responsesMode {
			initial := d.responseObject("in_progress", false, nil, true)
			if err := d.sendEvent("response.created", map[string]any{"response": initial}); err != nil {
				return err
			}
			if err := d.sendEvent("response.in_progress", map[string]any{"response": initial}); err != nil {
				return err
			}
		} else if err := d.writeChunk(chatChunk{ID: d.id, Object: "chat.completion.chunk", Created: d.created, Model: d.model,
			Choices: []chatChoice{{Delta: chatDelta{Role: "assistant"}}}}); err != nil {
			return err
		}
	}
	return d.flushHeld()
}

// flushHeld 把扣住的正文与调用按顺序写给客户端。
func (d *delivery) flushHeld() error {
	held := d.held
	d.held, d.heldBytes = nil, 0
	var pending strings.Builder // 扣住期间攒下的相邻正文增量合并成一条
	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		err := d.writeContent(pending.String())
		pending.Reset()
		return err
	}
	for _, it := range held {
		if it.call == nil {
			pending.WriteString(it.content)
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if err := d.writeCall(*it.call); err != nil {
			return err
		}
	}
	return flush()
}

func (d *delivery) writeContent(delta string) error {
	d.text.WriteString(delta)
	d.wireBytes += len(delta)
	if !d.responsesMode {
		return d.writeChunk(d.chunk(chatDelta{Content: delta}))
	}
	idx := -1
	for i, it := range d.items {
		if it.call == nil {
			idx = i
			break
		}
	}
	if idx < 0 { // 正文条目惰性开启；已有工具调用时追加在末尾，保持已发出的 output_index 稳定
		d.items = append(d.items, respItem{id: "msg_" + randomHex(12)})
		idx = len(d.items) - 1
		if err := d.sendEvent("response.output_item.added", map[string]any{"output_index": idx,
			"item": map[string]any{"id": d.items[idx].id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}); err != nil {
			return err
		}
		if err := d.sendEvent("response.content_part.added", map[string]any{"item_id": d.items[idx].id, "output_index": idx, "content_index": 0,
			"part": newOutputTextPart("")}); err != nil {
			return err
		}
	}
	return d.sendEvent("response.output_text.delta", map[string]any{
		"item_id": d.items[idx].id, "output_index": idx, "content_index": 0, "delta": delta, "logprobs": []any{},
	})
}

func (d *delivery) writeCall(tc toolCall) error {
	d.calls = append(d.calls, tc)
	if !d.responsesMode {
		return d.writeChunk(d.chunk(chatDelta{ToolCalls: []chatToolCall{{Index: len(d.calls) - 1, ID: tc.ID, Type: "function", Function: tc.Function}}}))
	}
	item := respItem{call: &tc, id: "fc_" + randomHex(12)}
	d.items = append(d.items, item)
	idx := len(d.items) - 1
	added := map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": tc.ID, "arguments": ""}
	if tc.nsNamespace != "" {
		added["namespace"], added["name"] = tc.nsNamespace, tc.callName()
	} else {
		added["name"] = tc.Function.Name
	}
	if err := d.sendEvent("response.output_item.added", map[string]any{"output_index": idx, "item": added}); err != nil {
		return err
	}
	return d.sendEvent("response.function_call_arguments.delta", map[string]any{
		"item_id": item.id, "output_index": idx, "delta": tc.Function.Arguments,
	})
}

func (d *delivery) chunk(delta chatDelta, finish ...string) chatChunk {
	ch := chatChunk{ID: d.id, Object: "chat.completion.chunk", Created: d.created, Model: d.model,
		Choices: []chatChoice{{Delta: delta}}}
	if len(finish) > 0 {
		ch.Choices[0].FinishReason = &finish[0]
	}
	return ch
}

func (d *delivery) writeChunk(ch chatChunk) error {
	b, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(&d.fw, "data: %s\n\n", b)
	return err
}

func (d *delivery) sendEvent(typ string, payload map[string]any) error {
	payload["type"] = typ
	payload["sequence_number"] = d.seq
	d.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(&d.fw, "event: %s\ndata: %s\n\n", typ, b)
	return err
}

// outputItems responses 输出数组的条目（流式按开启顺序；非流式在收尾时生成）
func (d *delivery) outputItems() []respItem {
	if d.stream {
		return d.items
	}
	var items []respItem
	if d.text.Len() > 0 {
		items = append(items, respItem{id: "msg_" + randomHex(12)})
	}
	for i := range d.calls {
		items = append(items, respItem{call: &d.calls[i], id: "fc_" + randomHex(12)})
	}
	return items
}

// responseObject responses 形态的 response 对象；initial = true 时是空壳（事件起始用）
func (d *delivery) responseObject(status string, incomplete bool, details *incompleteDetails, initial bool) map[string]any {
	output := []any{}
	var usagePtr any
	if !initial {
		for _, it := range d.outputItems() {
			output = append(output, it.final(status, d.text.String()))
		}
		usagePtr = d.responsesUsage()
	}
	resp := map[string]any{
		"id": d.id, "object": "response", "created_at": d.created, "status": status,
		"error": nil, "incomplete_details": details, "model": d.model, "output": output,
		"usage": usagePtr, "parallel_tool_calls": d.req.parallelCalls,
	}
	if incomplete {
		resp["incomplete"] = true
	}
	return resp
}

// finish 正常收尾（含 incomplete 的平稳结束），返回本次记账用量
func (d *delivery) finish(res *turnResult) (Usage, error) {
	u := d.usage()
	if !d.stream {
		var out []byte
		if d.responsesMode {
			resp := d.responseObject("completed", false, nil, false)
			out, _ = json.Marshal(resp)
		} else {
			content := d.text.String()
			msg := chatMessage{Role: "assistant", Content: &content}
			finish := "stop"
			if len(d.calls) > 0 {
				msg.ToolCalls = d.calls
				finish = "tool_calls"
				if content == "" {
					msg.Content = nil // 纯工具调用：OpenAI 规范此处是 null
				}
			}
			out, _ = json.Marshal(chatCompletion{ID: d.id, Object: "chat.completion", Created: d.created, Model: d.model,
				Choices: []chatCompletionChoice{{Message: msg, FinishReason: finish}}, Usage: d.chatUsage()})
		}
		d.w.Header().Set("Content-Type", "application/json")
		d.w.WriteHeader(http.StatusOK)
		_, err := d.w.Write(out)
		return u, err
	}
	if err := d.commit(); err != nil {
		return u, err
	}
	details := (*incompleteDetails)(nil)
	if res.incomplete {
		details = &incompleteDetails{Reason: "upstream_interrupted", Code: res.incompleteCode}
	}
	if !d.responsesMode {
		finish := "stop"
		if len(d.calls) > 0 {
			finish = "tool_calls"
		}
		final := d.chunk(chatDelta{}, finish)
		final.Incomplete, final.IncompleteDetails = res.incomplete, details
		if err := d.writeChunk(final); err != nil {
			return u, err
		}
		if d.includeUsage {
			usage := d.chunk(chatDelta{})
			usage.Choices, usage.Usage = []chatChoice{}, d.chatUsage()
			if err := d.writeChunk(usage); err != nil {
				return u, err
			}
		}
		_, err := fmt.Fprint(&d.fw, "data: [DONE]\n\n")
		return u, err
	}
	// responses：补齐已交付条目的 done 事件。中断时保留已交付的文本与调用，
	// 不再发 done / failed（严格的 Responses 代理会把它们当成传输失败而重连）
	if !res.incomplete {
		for idx, it := range d.items {
			if it.call == nil {
				pos := map[string]any{"item_id": it.id, "output_index": idx, "content_index": 0}
				done := map[string]any{"text": d.text.String(), "logprobs": []any{}}
				for k, v := range pos {
					done[k] = v
				}
				if err := d.sendEvent("response.output_text.done", done); err != nil {
					return u, err
				}
				part := map[string]any{"part": newOutputTextPart(d.text.String())}
				for k, v := range pos {
					part[k] = v
				}
				if err := d.sendEvent("response.content_part.done", part); err != nil {
					return u, err
				}
			} else {
				if err := d.sendEvent("response.function_call_arguments.done", map[string]any{
					"item_id": it.id, "output_index": idx, "name": it.call.callName(), "arguments": it.call.Function.Arguments}); err != nil {
					return u, err
				}
			}
			if err := d.sendEvent("response.output_item.done", map[string]any{"output_index": idx, "item": it.final("completed", d.text.String())}); err != nil {
				return u, err
			}
		}
	}
	event := map[string]any{"response": d.responseObject("completed", res.incomplete, details, false)}
	if res.incomplete {
		event["incomplete"] = true
	}
	return u, d.sendEvent("response.completed", event)
}
