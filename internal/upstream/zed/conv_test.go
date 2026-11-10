package zed

import (
	"encoding/json"
	"strings"
	"testing"
)

func zedParse(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("解析 JSON 失败: %v\n%s", err, raw)
	}
	return m
}

func TestZedChatToResponses(t *testing.T) {
	src := zedParse(t, `{
		"model": "ignored",
		"messages": [
			{"role": "system", "content": "你是助手"},
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": "在", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"北京\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
			{"role": "user", "content": [
				{"type": "text", "text": "看图"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,xxx"}}
			]}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}}],
		"max_tokens": 512,
		"temperature": 0.7
	}`)
	out, err := zedChatToResponses(src, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "gpt-5.4" {
		t.Fatalf("model 应被替换: %v", out["model"])
	}
	if out["instructions"] != "你是助手" {
		t.Fatalf("system 应聚合为 instructions: %v", out["instructions"])
	}
	if n, _ := zedInt(out["max_output_tokens"]); n != 512 {
		t.Fatalf("max_tokens 应转 max_output_tokens: %v", out["max_output_tokens"])
	}
	items := zedSlice(out["input"])
	if len(items) != 5 { // user 文本 / assistant 文本 / assistant function_call / tool 输出 / user 多部件
		t.Fatalf("input 条目数 = %d: %s", len(items), mustJSON(out))
	}
	first := zedMap(items[0])
	if zedStr(first["role"]) != "user" {
		t.Fatalf("首条应为 user message: %s", mustJSON(first))
	}
	if zedStr(zedMap(items[1])["role"]) != "assistant" {
		t.Fatalf("assistant 文本应保留为 message: %s", mustJSON(items[1]))
	}
	call := zedMap(items[2])
	if zedStr(call["type"]) != "function_call" || zedStr(call["call_id"]) != "call_1" ||
		zedStr(call["name"]) != "get_weather" {
		t.Fatalf("tool_calls 应转 function_call: %s", mustJSON(call))
	}
	outItem := zedMap(items[3])
	if zedStr(outItem["type"]) != "function_call_output" || zedStr(outItem["call_id"]) != "call_1" || zedStr(outItem["output"]) != "晴" {
		t.Fatalf("tool 消息应转 function_call_output: %s", mustJSON(outItem))
	}
	last := zedMap(items[4])
	parts := zedSlice(last["content"])
	if len(parts) != 2 || zedStr(zedMap(parts[1])["type"]) != "input_image" {
		t.Fatalf("图片部件应保留: %s", mustJSON(last))
	}
	tools := zedSlice(out["tools"])
	if len(tools) != 1 || zedStr(zedMap(tools[0])["name"]) != "get_weather" || zedMap(tools[0])["parameters"] == nil {
		t.Fatalf("tools 应转为扁平形态: %s", mustJSON(tools))
	}
	if _, ok := out["temperature"]; !ok {
		t.Fatal("temperature 应透传")
	}
}

func TestZedChatToAnthropic(t *testing.T) {
	src := zedParse(t, `{
		"messages": [
			{"role": "system", "content": "sys1"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "ok", "tool_calls": [
				{"id": "call_9", "type": "function", "function": {"name": "f", "arguments": "{\"a\":1}"}}
			]},
			{"role": "tool", "tool_call_id": "call_9", "content": "result"}
		],
		"tools": [{"type": "function", "function": {"name": "f", "input_schema_x": 1, "description": "d", "parameters": {"type": "object"}}}],
		"max_completion_tokens": 1000
	}`)
	out, err := zedChatToAnthropic(src, "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := zedInt(out["max_tokens"]); n != 1000 {
		t.Fatalf("max_completion_tokens 应映射: %v", out["max_tokens"])
	}
	sys := zedSlice(out["system"])
	if len(sys) != 1 || zedStr(zedMap(sys[0])["text"]) != "sys1" {
		t.Fatalf("system 应转文本块数组: %s", mustJSON(out["system"]))
	}
	msgs := zedSlice(out["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages = %d: %s", len(msgs), mustJSON(msgs))
	}
	// assistant: text + tool_use
	assistant := zedMap(msgs[1])
	blocks := zedSlice(assistant["content"])
	if zedStr(zedMap(blocks[0])["type"]) != "text" {
		t.Fatalf("assistant 文本块: %s", mustJSON(blocks))
	}
	tu := zedMap(blocks[1])
	if zedStr(tu["type"]) != "tool_use" || zedStr(tu["id"]) != "call_9" || zedStr(tu["name"]) != "f" {
		t.Fatalf("tool_calls 应转 tool_use: %s", mustJSON(tu))
	}
	if a, ok := zedInt(zedMap(tu["input"])["a"]); !ok || a != 1 {
		t.Fatalf("arguments 应解析为 input 对象: %s", mustJSON(tu["input"]))
	}
	// tool 消息 → user 的 tool_result
	toolMsg := zedMap(msgs[2])
	if zedStr(toolMsg["role"]) != "user" {
		t.Fatalf("tool 消息应转 user 角色: %s", mustJSON(toolMsg))
	}
	tr := zedMap(zedSlice(toolMsg["content"])[0])
	if zedStr(tr["type"]) != "tool_result" || zedStr(tr["tool_use_id"]) != "call_9" || tr["is_error"] != false {
		t.Fatalf("tool_result 块: %s", mustJSON(tr))
	}
	tools := zedSlice(out["tools"])
	if len(tools) != 1 || zedMap(tools[0])["input_schema"] == nil {
		t.Fatalf("tools 应带 input_schema: %s", mustJSON(tools))
	}
}

func TestZedChatToAnthropicDefaultMaxTokens(t *testing.T) {
	out, err := zedChatToAnthropic(zedParse(t, `{"messages":[{"role":"user","content":"x"}]}`), "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := zedInt(out["max_tokens"]); n != zedAnthropicDefaultMaxTokens {
		t.Fatalf("缺省 max_tokens 应为 %d: %v", zedAnthropicDefaultMaxTokens, out["max_tokens"])
	}
}

func TestZedImageSource(t *testing.T) {
	src := zedImageSource("data:image/png;base64,aGVsbG8=")
	if src == nil || zedStr(src["type"]) != "base64" || zedStr(src["media_type"]) != "image/png" || zedStr(src["data"]) != "aGVsbG8=" {
		t.Fatalf("data URL 解析: %s", mustJSON(src))
	}
	if src := zedImageSource("https://x/y.png"); src == nil || zedStr(src["type"]) != "url" {
		t.Fatalf("http URL 解析: %s", mustJSON(src))
	}
	if zedImageSource("ftp://x") != nil || zedImageSource("data:bad") != nil {
		t.Fatal("不支持的形态应返回 nil")
	}
}

// ── 回程翻译器：responses 事件 → chat 分片 ────────────────────────────────

func TestZedChatFromResponsesStream(t *testing.T) {
	var sb strings.Builder
	tr := newZedChatFromResponses(&sb, "gpt-5.4", false)
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message","role":"assistant"}}`,
		`{"type":"response.output_text.delta","item_id":"m1","delta":"你"}`,
		`{"type":"response.output_text.delta","item_id":"m1","delta":"好"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc1","type":"function_call","call_id":"call_7","name":"f"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{\"a\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"1}"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":2}}}}`,
	}
	for _, ev := range events {
		if err := tr.event(zedParse(t, ev)); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := tr.finish()
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`"role":"assistant"`, `"content":"你"`, `"content":"好"`,
		`"tool_calls":[{"function":{"arguments":"","name":"f"},"id":"call_7","index":0,"type":"function"}]`,
		`"arguments":"{\"a\":"`, `"finish_reason":"tool_calls"`, `data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("分片输出缺少 %q:\n%s", want, out)
		}
	}
	if usage.Prompt != 10 || usage.Completion != 4 || usage.Cached != 2 {
		t.Fatalf("用量提取不对: %+v", usage)
	}
	// 未开启 emitUsage：不应有独立 usage 帧
	if strings.Contains(out, `"usage":{"prompt_tokens":10`) {
		t.Fatal("未要求 include_usage 时不应发 usage 帧")
	}
}

func TestZedChatFromAnthropicStream(t *testing.T) {
	var sb strings.Builder
	tr := newZedChatFromAnthropic(&sb, "claude-sonnet-4.5", true)
	events := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"嗨"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"calc"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"2+2"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`,
		`{"type":"message_stop"}`,
	}
	for _, ev := range events {
		if err := tr.event(zedParse(t, ev)); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := tr.finish()
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`"role":"assistant"`, `"content":"嗨"`,
		`"tool_calls":[{"function":{"arguments":"","name":"calc"},"id":"tu_1","index":0,"type":"function"}]`,
		`"arguments":"2+2"`, `"finish_reason":"tool_calls"`,
		`"usage":{"completion_tokens":6,"prompt_tokens":7`, `data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("分片输出缺少 %q:\n%s", want, out)
		}
	}
	if usage.Prompt != 7 || usage.Cached != 3 || usage.Completion != 6 {
		t.Fatalf("用量提取不对: %+v", usage)
	}
}

// Zed 的 Anthropic 流缺 message_start / message_stop 时应能补齐（ccLoad 实测场景）
func TestZedChatFromAnthropicSynthFinish(t *testing.T) {
	var sb strings.Builder
	tr := newZedChatFromAnthropic(&sb, "claude-sonnet-4.5", false)
	tr.event(zedParse(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`))
	usage, err := tr.finish()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Prompt != 0 || usage.Completion != 0 {
		t.Fatalf("无 usage 事件时应为零值: %+v", usage)
	}
	out := sb.String()
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("应补齐首片与终片:\n%s", out)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
