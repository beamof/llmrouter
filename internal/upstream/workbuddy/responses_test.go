package workbuddy

// /v1/responses 协议转换测试：入站翻译（responses→chat 出站体）、流式事件渲染
// （chat 分片→responses SSE）、非流式聚合（response 对象）。链路复用 wbFakeUpstream。

import (
	"encoding/json"
	"llmrouter/internal/wire"
	"strings"
	"testing"
)

func TestWBResponsesToChat(t *testing.T) {
	out, err := wire.ResponsesToChat([]byte(`{
		"model":"glm-5.0","instructions":"要简短","max_output_tokens":50,
		"temperature":0.3,
		"reasoning":{"effort":"high"},
		"tools":[{"type":"function","name":"get_time","description":"取时间","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"get_time"},
		"store":false,"include":["reasoning.encrypted_content"],"previous_response_id":"resp_x",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"几点了"},
				{"type":"input_image","image_url":"https://x/c.png"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"旧思考"}]},
			{"type":"function_call","call_id":"call_1","name":"get_time","arguments":"{\"tz\":\"UTC\"}"},
			{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{\"tz\":\"CST\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"12:00"}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "glm-5.0" || out["max_tokens"] != int64(50) || out["temperature"] != 0.3 {
		t.Fatalf("顶层字段搬运不对: %#v", out)
	}
	if _, ok := out["store"]; ok {
		t.Fatal("store 应被丢弃")
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 4 { // system + user + assistant(tool_calls) + tool
		t.Fatalf("消息数不对: %#v", msgs)
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "要简短" {
		t.Fatalf("instructions 应前置 system: %#v", m0)
	}
	m1 := msgs[1].(map[string]any)
	parts := m1["content"].([]any) // 图文混排保留部件数组
	if parts[0].(map[string]any)["text"] != "几点了" || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("user 内容转换不对: %#v", m1)
	}
	m2 := msgs[2].(map[string]any)
	calls := m2["tool_calls"].([]any) // 相邻 function_call 聚成一条 assistant
	if m2["role"] != "assistant" || len(calls) != 2 ||
		calls[0].(map[string]any)["id"] != "call_1" ||
		calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != `{"tz":"CST"}` {
		t.Fatalf("function_call 聚合不对: %#v", m2)
	}
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" || m3["content"] != "12:00" {
		t.Fatalf("function_call_output 转换不对: %#v", m3)
	}
	tools := out["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if tools[0].(map[string]any)["type"] != "function" || fn["name"] != "get_time" || fn["description"] != "取时间" {
		t.Fatalf("tools 转换不对: %#v", tools)
	}
	if out["tool_choice"] == nil {
		t.Fatal("tool_choice 应保留（tools 存活时）")
	}
	if r, ok := out["reasoning"].(map[string]any); !ok || r["effort"] != "high" {
		t.Fatalf("reasoning.effort 应透传给档位归一化: %#v", out["reasoning"])
	}
}

func TestWBResponsesToChatDrops(t *testing.T) {
	// 非函数工具全被丢弃时 tools/tool_choice 一并不出现在出站体
	out, err := wire.ResponsesToChat([]byte(`{
		"model":"glm-5.0","input":"hi",
		"tools":[{"type":"web_search"}],"tool_choice":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["tools"]; ok {
		t.Fatal("非函数工具应整体丢弃")
	}
	if _, ok := out["tool_choice"]; ok {
		t.Fatal("tools 丢弃时 tool_choice 不应保留")
	}
	if _, err := wire.ResponsesToChat([]byte(`{"model":"glm-5.0","store":false}`)); err == nil {
		t.Fatal("缺 input 应失败")
	}
	if _, err := wire.ResponsesToChat([]byte(`not json`)); err == nil {
		t.Fatal("非 JSON 应失败")
	}
}

// wbRespSSEFixture 一段带思考、正文与工具调用的上游 chat 分片流
const wbRespSSEFixture = "data: " + `{"choices":[{"delta":{"reasoning_content":"思考"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"content":"你"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"content":"好"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"get_time","arguments":"{\"tz\":"}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: " + `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":5}}}` + "\n\n" +
	"data: [DONE]\n\n"

// sseEvents 解析 SSE 文本为 (事件名, data JSON) 序列

func sseEvents(t *testing.T, body string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		ev, dataLine := "", ""
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				ev = rest
			}
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				dataLine = rest
			}
		}
		if dataLine == "" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(dataLine), &d); err != nil {
			t.Fatalf("SSE data 不是 JSON: %v: %s", err, dataLine)
		}
		out = append(out, struct {
			Event string
			Data  map[string]any
		}{ev, d})
	}
	return out
}
