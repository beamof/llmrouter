package zed

import (
	"llmrouter/internal/upstream"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 信封装配 ───────────────────────────────────────────────────────────────

func TestZedBuildEnvelopeGPTChat(t *testing.T) {
	// chat 客户端转 responses 时 reasoning_effort 被丢弃：plan.effort 应为空
	body := `{"model":"gpt-5.4","stream":false,"reasoning_effort":"high","messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好"}],"max_tokens":100}`
	raw, plan, err := zedBuildEnvelope([]byte(body), false, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if plan.provider != zedProviderOpenAI {
		t.Fatalf("provider = %s", plan.provider)
	}
	env := zedParse(t, string(raw))
	if env["provider"] != zedProviderOpenAI || env["model"] != "gpt-5.4" || env["intent"] != "user_prompt" {
		t.Fatalf("信封形态: %s", raw)
	}
	if env["thread_id"] == "" || env["prompt_id"] == "" {
		t.Fatal("信封应有 thread_id/prompt_id")
	}
	pr := zedMap(env["provider_request"])
	if pr == nil || pr["stream"] != true {
		t.Fatalf("gpt 线路应强制 stream:true: %s", mustJSON(pr))
	}
	if pr["instructions"] != "sys" {
		t.Fatalf("system 应聚合为 instructions: %s", mustJSON(pr))
	}
	input := zedSlice(pr["input"])
	if len(input) != 1 || zedStr(zedMap(input[0])["role"]) != "user" {
		t.Fatalf("input 应含 user message: %s", mustJSON(pr["input"]))
	}
	if n, _ := zedInt(pr["max_output_tokens"]); n != 100 {
		t.Fatalf("max_output_tokens 保留: %v", pr["max_output_tokens"])
	}
	if plan.effort != "" {
		t.Fatalf("chat→responses 丢弃档位，实发应为空: %q", plan.effort)
	}
}

func TestZedBuildEnvelopeGPTResponsesPassthrough(t *testing.T) {
	// responses 客户端 + gpt 模型：原始 body 保真透传（instructions 等字段不丢）
	body := `{"model":"gpt-5.4","instructions":"codex system","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"f","parameters":{}}],"reasoning":{"effort":"high"}}`
	raw, plan, err := zedBuildEnvelope([]byte(body), true, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if plan.provider != zedProviderOpenAI {
		t.Fatal("provider")
	}
	env := zedParse(t, string(raw))
	pr := zedMap(env["provider_request"])
	if pr["instructions"] != "codex system" {
		t.Fatalf("instructions 应保留: %s", mustJSON(pr))
	}
	if zedStr(zedMap(zedSlice(pr["tools"])[0])["name"]) != "f" {
		t.Fatalf("responses tools 应原样保留: %s", mustJSON(pr["tools"]))
	}
	if zedStr(zedMap(pr["reasoning"])["effort"]) != "high" {
		t.Fatalf("reasoning 应保留: %s", mustJSON(pr))
	}
	if plan.effort != "high" {
		t.Fatalf("实发档位应记为客户端的 high: %q", plan.effort)
	}
	if pr["stream"] != true {
		t.Fatal("应强制流式")
	}
}

func TestZedBuildEnvelopeClaudeChat(t *testing.T) {
	body := `{"model":"claude-sonnet-4.5","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],"max_tokens":300}`
	raw, plan, err := zedBuildEnvelope([]byte(body), false, "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if plan.provider != zedProviderAnthropic {
		t.Fatal("provider")
	}
	env := zedParse(t, string(raw))
	pr := zedMap(env["provider_request"])
	if _, has := pr["stream"]; has {
		t.Fatal("anthropic 线路不应带 stream 标志")
	}
	if n, _ := zedInt(pr["max_tokens"]); n != 300 {
		t.Fatalf("max_tokens: %v", pr["max_tokens"])
	}
	if zedStr(zedMap(zedSlice(pr["system"])[0])["text"]) != "s" {
		t.Fatalf("system 应为块数组: %s", mustJSON(pr["system"]))
	}
}

func TestZedBuildEnvelopeClaudeThinkingBudget(t *testing.T) {
	// 未显式给 max_tokens 且 thinking 预算装不进默认值 → 抬高 max_tokens = 预算 + 8192
	body := `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":10000}}`
	raw, _, err := zedBuildEnvelope([]byte(body), false, "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	pr := zedMap(zedParse(t, string(raw))["provider_request"])
	if n, _ := zedInt(pr["max_tokens"]); n != 10000+zedAnthropicDefaultMaxTokens {
		t.Fatalf("max_tokens 应抬高: %v", pr["max_tokens"])
	}
	// 预算本来就能装进默认 max_tokens（8192 > 4096）时不干预
	body = `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":4096}}`
	raw, _, _ = zedBuildEnvelope([]byte(body), false, "claude-sonnet-4.5")
	pr = zedMap(zedParse(t, string(raw))["provider_request"])
	if n, _ := zedInt(pr["max_tokens"]); n != zedAnthropicDefaultMaxTokens {
		t.Fatalf("够用时不应干预: %v", pr["max_tokens"])
	}
	// 显式给了一个装不下预算的 max_tokens → 报错（静默改写显式声明更糟）
	body = `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}],"max_tokens":100,"thinking":{"type":"enabled","budget_tokens":4096}}`
	if _, _, err := zedBuildEnvelope([]byte(body), false, "claude-sonnet-4.5"); err == nil {
		t.Fatal("显式冲突的预算应报错")
	}
}

func TestZedBuildEnvelopeUnsupportedModel(t *testing.T) {
	if _, _, err := zedBuildEnvelope([]byte(`{"messages":[]}`), false, "gemini-3-pro"); err == nil {
		t.Fatal("gemini 模型应报不支持")
	}
	if _, _, err := zedBuildEnvelope([]byte(`{"messages":[]}`), false, "unknown-model"); err == nil {
		t.Fatal("未知模型应报不支持")
	}
}

func TestZedNormalizeResponsesItems(t *testing.T) {
	body := zedParse(t, `{"model":"gpt-5.4","stream":false,"input":[
		{"type":"agent_message","content":[{"type":"text","text":"早"}],"author":"a","recipient":"r"},
		{"type":"message","role":"developer","content":"dev"},
		{"type":"message","role":"assistant","content":"答"},
		{"type":"compaction","summary":"..."},
		{"type":"reasoning","summary":[]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"有摘要"}],"encrypted_content":"enc"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"note":null}
	],"include":["reasoning.encrypted_content","other"],"store":false}`)
	zedNormalizeResponses(body)
	if body["stream"] != true {
		t.Fatal("应强制 stream")
	}
	items := zedSlice(body["input"])
	if len(items) != 5 { // agent_message / developer / assistant / 带摘要 reasoning / user
		t.Fatalf("条目数 = %d: %s", len(items), mustJSON(items))
	}
	m0 := zedMap(items[0])
	if zedStr(m0["type"]) != "message" || zedStr(m0["role"]) != "user" || m0["author"] != nil {
		t.Fatalf("agent_message 折算: %s", mustJSON(m0))
	}
	m1 := zedMap(items[1])
	if zedStr(m1["role"]) != "system" || zedStr(zedMap(zedSlice(m1["content"])[0])["type"]) != "input_text" {
		t.Fatalf("developer 折算: %s", mustJSON(m1))
	}
	m2 := zedMap(items[2])
	if zedStr(zedMap(zedSlice(m2["content"])[0])["type"]) != "output_text" {
		t.Fatalf("assistant 文本应为 output_text: %s", mustJSON(m2))
	}
	m3 := zedMap(items[3])
	if zedStr(m3["type"]) != "reasoning" || m3["encrypted_content"] != nil {
		t.Fatalf("带摘要 reasoning 保留但剥密文: %s", mustJSON(m3))
	}
	m4 := zedMap(items[4])
	if _, has := m4["note"]; has {
		t.Fatal("null 值字段应删除")
	}
	inc := zedSlice(body["include"])
	if len(inc) != 1 || inc[0] != "other" {
		t.Fatalf("include 应去掉 reasoning.*: %s", mustJSON(body["include"]))
	}
}

// ── 帧分类 ────────────────────────────────────────────────────────────────

func TestZedClassifyFrame(t *testing.T) {
	cases := []struct {
		line string
		want zedFrameClass
	}{
		{`{"status":"queued"}`, zedFrameInProgress},
		{`{"status":"in_progress"}`, zedFrameInProgress},
		{`{"status":{"progress":"halfway"}}`, zedFrameInProgress},
		{`{"status":"stream_ended"}`, zedFrameEnded},
		{`{"status":"failed"}`, zedFrameFailed},
		{`{"status":"error"}`, zedFrameFailed},
		{`{"status":{"failed":{"code":"rate_limited","message":"too many"}}}`, zedFrameFailed},
		{`{"event":{"type":"response.output_text.delta","delta":"x"}}`, zedFrameEvent},
		{`{"type":"response.output_text.delta","delta":"x"}`, zedFrameEvent},
		{`not json`, zedFrameSkip},
	}
	for _, c := range cases {
		class, _, _ := zedClassifyFrame([]byte(c.line))
		if class != c.want {
			t.Fatalf("%s → %d, want %d", c.line, class, c.want)
		}
	}
	// failed 帧带出错误信息
	_, _, msg := zedClassifyFrame([]byte(`{"status":{"failed":{"code":"rate_limited","message":"too many"}}}`))
	if msg != "rate_limited: too many" {
		t.Fatalf("failed 信息: %q", msg)
	}
	// event 帧解析出事件对象
	class, ev, _ := zedClassifyFrame([]byte(`{"event":{"type":"response.output_text.delta","delta":"x"}}`))
	if class == zedFrameEvent && zedStr(ev["delta"]) != "x" {
		t.Fatalf("事件对象: %s", mustJSON(ev))
	}
}

// ── 回程矩阵：zedRelay 的六条路径 ─────────────────────────────────────────

// zedAnthropicSSE 一段典型的 Zed Anthropic 流（event 帧 + 状态帧）
const zedAnthropicSSE = `data: {"status":"queued"}

data: {"event":{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":2}}}}

data: {"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}}

data: {"event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}

data: {"status":"stream_ended"}

`

// zedResponsesSSE 一段典型的 Zed Responses 流（gpt 线路）
const zedResponsesSSE = `data: {"status":"queued"}

data: {"event":{"type":"response.output_text.delta","delta":"嗨"}}

data: {"event":{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}}}

data: {"status":"stream_ended"}

`

func zedRelayRun(t *testing.T, s *Adapter, up string, plan *zedWirePlan, wantStream, responsesMode, includeUsage bool) (*httptest.ResponseRecorder, Usage, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	u, _, err := s.relay(strings.NewReader(up), rec, plan, plan.model, wantStream, responsesMode, includeUsage)
	return rec, u, err
}

func TestZedRelayClaudeChatStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	rec, u, err := zedRelayRun(t, s, zedAnthropicSSE, plan, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"role":"assistant"`, `"content":"好"`, `"finish_reason":"stop"`, `data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if rec.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("流式头: %s", rec.Header().Get("Content-Type"))
	}
	if u.Prompt != 5 || u.Cached != 2 || u.Completion != 3 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestZedRelayClaudeChatNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	rec, u, err := zedRelayRun(t, s, zedAnthropicSSE, plan, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	res := zedParse(t, rec.Body.String())
	if res["object"] != "chat.completion" {
		t.Fatalf("应聚合为 chat.completion: %s", rec.Body.String())
	}
	choices := zedSlice(res["choices"])
	msg := zedMap(zedMap(choices[0])["message"])
	if zedStr(msg["content"]) != "好" || zedStr(zedMap(choices[0])["finish_reason"]) != "stop" {
		t.Fatalf("聚合内容: %s", rec.Body.String())
	}
	usage := zedMap(res["usage"])
	if n, _ := zedInt(usage["prompt_tokens"]); n != 5 {
		t.Fatalf("聚合用量: %s", rec.Body.String())
	}
	if u.Completion != 3 {
		t.Fatalf("记账用量: %+v", u)
	}
}

func TestZedRelayClaudeResponsesStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	rec, u, err := zedRelayRun(t, s, zedAnthropicSSE, plan, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_text.delta", "event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if u.Prompt != 5 || u.Completion != 3 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestZedRelayClaudeResponsesNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	rec, _, err := zedRelayRun(t, s, zedAnthropicSSE, plan, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	res := zedParse(t, rec.Body.String())
	if res["object"] != "response" || res["status"] != "completed" {
		t.Fatalf("应聚合为 response 对象: %s", rec.Body.String())
	}
	// wire.Aggregator.responsesResult 的 output 里应有 message
	found := false
	for _, raw := range zedSlice(res["output"]) {
		if zedStr(zedMap(raw)["type"]) == "message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("output 应含 message: %s", rec.Body.String())
	}
}

func TestZedRelayGPTChatStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderOpenAI, model: "gpt-5.4"}
	rec, u, err := zedRelayRun(t, s, zedResponsesSSE, plan, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"嗨"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if u.Prompt != 8 || u.Cached != 1 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestZedRelayGPTChatNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderOpenAI, model: "gpt-5.4"}
	rec, u, err := zedRelayRun(t, s, zedResponsesSSE, plan, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	res := zedParse(t, rec.Body.String())
	if res["object"] != "chat.completion" {
		t.Fatalf("应聚合为 chat.completion: %s", rec.Body.String())
	}
	if u.Prompt != 8 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestZedRelayGPTResponsesStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderOpenAI, model: "gpt-5.4"}
	rec, u, err := zedRelayRun(t, s, zedResponsesSSE, plan, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.output_text.delta", "event: response.completed", `"delta":"嗨"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if u.Prompt != 8 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestZedRelayGPTResponsesNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderOpenAI, model: "gpt-5.4"}
	rec, u, err := zedRelayRun(t, s, zedResponsesSSE, plan, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	res := zedParse(t, rec.Body.String())
	if res["id"] != "resp_1" || res["status"] != "completed" {
		t.Fatalf("应回传 response.completed 的对象: %s", rec.Body.String())
	}
	if u.Prompt != 8 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

// 流失败（未写出任何字节）→ 返回错误供候选循环换下一家

func TestZedRelayFailedBeforeStart(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	up := `data: {"status":{"failed":{"code":"rate_limited","message":"too many"}}}` + "\n\n"
	rec, _, err := zedRelayRun(t, s, up, plan, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("应返回错误: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("未写出任何字节: %s", rec.Body.String())
	}
}

// 已写出部分内容后失败 → 尽量收尾并报告错误（不 panic、不静默吞掉）

func TestZedRelayFailedMidStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	up := `data: {"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半"}}}

data: {"status":{"failed":{"code":"boom","message":"中途挂"}}}

`
	rec, _, err := zedRelayRun(t, s, up, plan, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("应报告错误: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"半"`) {
		t.Fatal("已写内容应保留")
	}
}

// 上游直接关流（无 stream_ended）→ 按正常结束收尾

func TestZedRelayEOFWithoutEnd(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	plan := &zedWirePlan{provider: zedProviderAnthropic, model: "claude-sonnet-4.5"}
	up := `data: {"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"短"}}}

` // 无 stream_ended，流直接结束
	rec, _, err := zedRelayRun(t, s, up, plan, true, false, false)
	if err != nil {
		t.Fatalf("EOF 应按正常结束: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatal("应补终片")
	}
}

// ── e2e：走完整代理链路 ───────────────────────────────────────────────────
