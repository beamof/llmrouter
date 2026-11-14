package app

// /v1/responses 协议转换测试：入站翻译（responses→chat 出站体）、流式事件渲染
// （chat 分片→responses SSE）、非流式聚合（response 对象）。链路复用 wbFakeUpstream。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestWBResponsesStream(t *testing.T) {
	var gotBody string
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, wbSSEFixture)
	})
	s, app := newTestServer(t)
	s.store.SetResponsesConvert(true) // 转换默认关闭，本测试验证转换链路
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/responses",
		`{"model":"wb-p/glm-5.0","input":"hi","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("应为 SSE: %s", resp.Header.Get("Content-Type"))
	}

	// 出站体：input 字符串翻成了 user 消息，强制流式
	var outbound map[string]any
	if err := json.Unmarshal([]byte(gotBody), &outbound); err != nil {
		t.Fatalf("出站体不是 JSON: %v: %s", err, gotBody)
	}
	if outbound["stream"] != true {
		t.Fatal("上游应强制流式")
	}
	msgs := outbound["messages"].([]any)
	um := msgs[len(msgs)-1].(map[string]any) // 首条是注入的兜底 system，末条才是 input 翻译出的 user
	if um["role"] != "user" || um["content"] != "hi" {
		t.Fatalf("input 字符串应翻成 user 消息: %#v", outbound["messages"])
	}

	events := sseEvents(t, body)
	if len(events) == 0 || events[0].Event != "response.created" {
		t.Fatalf("首事件应是 response.created: %s", body)
	}
	var saw struct {
		reasoningDelta, textDelta, fcDelta bool
		completed                          map[string]any
	}
	var textAll, argsAll string
	for _, ev := range events {
		switch ev.Event {
		case "response.reasoning_summary_text.delta":
			saw.reasoningDelta = true
		case "response.output_text.delta":
			saw.textDelta = true
			textAll += ev.Data["delta"].(string)
		case "response.function_call_arguments.delta":
			saw.fcDelta = true
			argsAll += ev.Data["delta"].(string)
		case "response.completed":
			saw.completed = ev.Data["response"].(map[string]any)
		}
	}
	if !saw.reasoningDelta || !saw.textDelta || !saw.fcDelta {
		t.Fatalf("三类增量事件缺失: %s", body)
	}
	if textAll != "你好" || argsAll != `{"tz":"UTC"}` {
		t.Fatalf("增量聚合不对: text=%q args=%q", textAll, argsAll)
	}
	if saw.completed == nil {
		t.Fatalf("缺少 response.completed: %s", body)
	}
	items := saw.completed["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("output 应有 reasoning/message/function_call 三个 item: %s", body)
	}
	fc := items[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" ||
		fc["name"] != "get_time" || fc["arguments"] != `{"tz":"UTC"}` {
		t.Fatalf("function_call item 不对: %#v", fc)
	}
	usage := saw.completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) ||
		usage["total_tokens"] != float64(18) {
		t.Fatalf("usage 映射不对: %#v", usage)
	}
	snap := string(s.store.SnapshotJSON())
	if !strings.Contains(snap, `"prompt_tokens":11`) || !strings.Contains(snap, `"cached_tokens":5`) {
		t.Fatalf("流式 usage 未记账: %s", snap)
	}
}

func TestWBResponsesNonStream(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, wbSSEFixture)
	})
	s, app := newTestServer(t)
	s.store.SetResponsesConvert(true) // 转换默认关闭，本测试验证转换链路
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/responses",
		`{"model":"wb-p/glm-5.0","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("非流式应为 JSON: %s", resp.Header.Get("Content-Type"))
	}
	var v struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("非流式响应应是 response 对象: %v: %s", err, body)
	}
	if v.Object != "response" || v.Status != "completed" {
		t.Fatalf("对象形状不对: %s", body)
	}
	if len(v.Output) != 3 {
		t.Fatalf("output 应有 3 个 item: %s", body)
	}
	if v.Output[0].Type != "reasoning" || v.Output[0].Summary[0].Text != "思考" {
		t.Fatalf("reasoning item 不对: %s", body)
	}
	if v.Output[1].Type != "message" || v.Output[1].Content[0].Text != "你好" {
		t.Fatalf("message item 不对: %s", body)
	}
	if v.Output[2].Type != "function_call" || v.Output[2].CallID != "call_9" ||
		v.Output[2].Arguments != `{"tz":"UTC"}` || v.Output[2].Name != "get_time" {
		t.Fatalf("function_call item 不对: %s", body)
	}
	if v.Usage.InputTokens != 11 || v.Usage.OutputTokens != 7 || v.Usage.TotalTokens != 18 {
		t.Fatalf("usage 不对: %s", body)
	}
	snap := string(s.store.SnapshotJSON())
	if !strings.Contains(snap, `"prompt_tokens":11`) {
		t.Fatalf("非流式 usage 未记账: %s", snap)
	}
}

func TestWBResponsesIncomplete(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"截断"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
			"",
		}, "\n\n"))
	})
	s, app := newTestServer(t)
	s.store.SetResponsesConvert(true) // 转换默认关闭，本测试验证转换链路
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	// 流式：response.incomplete 事件
	_, body := postJSON(t, app, "/v1/responses",
		`{"model":"wb-p/glm-5.0","input":"hi","stream":true}`)
	found := false
	for _, ev := range sseEvents(t, body) {
		if ev.Event == "response.incomplete" {
			found = true
			r := ev.Data["response"].(map[string]any)
			if r["status"] != "incomplete" ||
				r["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
				t.Fatalf("incomplete 细节不对: %s", body)
			}
		}
		if ev.Event == "response.completed" {
			t.Fatalf("length 截断不应发 completed: %s", body)
		}
	}
	if !found {
		t.Fatalf("缺少 response.incomplete: %s", body)
	}

	// 非流式：status=incomplete 的 response 对象
	_, body = postJSON(t, app, "/v1/responses", `{"model":"wb-p/glm-5.0","input":"hi"}`)
	var v struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Status != "incomplete" || v.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("非流式 incomplete 不对: %s", body)
	}
}

// 直连模型在 /responses 下失败时不兜底：WorkBuddy 的 429 原样回给客户端，
// 即使同组/其他位置还有可用的普通模型也不切换。
func TestDirectResponsesNoFallback(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":6004,"message":"限流"}`)
	})
	s, app := newTestServer(t)
	s.store.SetResponsesConvert(true) // 开启转换后 wb 模型才支持 responses
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	nativeHit := false
	fallback := http.NewServeMux()
	fallback.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		nativeHit = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[]}`)
	})
	fb := httptest.NewServer(fallback)
	defer fb.Close()
	addProviderModel(t, s, fb.URL, "glm-5.0", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/responses",
		`{"model":"wb-p/glm-5.0","input":"hi"}`)
	if resp.StatusCode != 429 || !strings.Contains(body, "限流") {
		t.Fatalf("直连应透传上游 429: %d %s", resp.StatusCode, body)
	}
	if nativeHit {
		t.Fatal("直连失败不应切换到其他模型")
	}
}

// responses 协议转换默认关闭：路由组会跳过仅支持 chat 的 WorkBuddy 成员，
// 改用组内原生支持 responses 的模型。
func TestResponsesGroupSkipsChatOnlyCandidate(t *testing.T) {
	wbCalled := false
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		wbCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"wb\"}}]}\n\ndata: [DONE]\n\n")
	})
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("responses 请求应走原生 responses 端点, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer native.Close()

	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "shared", &WorkBuddyCred{UID: "u", AccessToken: "at-1"}) // 优先级 10
	addProviderModel(t, s, native.URL, "shared", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/responses", `{"model":"auto","input":"hi"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "resp_native") {
		t.Fatalf("组内应改走原生 responses 模型: %d %s", resp.StatusCode, body)
	}
	if wbCalled {
		t.Fatal("转换默认关闭时不应把 responses 请求发给 WorkBuddy")
	}
}

// 直连一个仅支持 chat 的模型时，/v1/responses 直接报错，不切换到其他模型。
func TestDirectResponsesUnsupportedNoFallback(t *testing.T) {
	wbCalled := false
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		wbCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"wb\"}}]}\n\ndata: [DONE]\n\n")
	})
	nativeHit := false
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHit = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_other","object":"response","status":"completed","output":[]}`)
	}))
	defer native.Close()

	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "wb-only", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	addProviderModel(t, s, native.URL, "native-model", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/responses", `{"model":"wb-p/wb-only","input":"hi"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "不支持当前接入协议") {
		t.Fatalf("应直接返回协议不支持: %d %s", resp.StatusCode, body)
	}
	if wbCalled || nativeHit {
		t.Fatal("协议不支持时不应调用任何上游")
	}
}

// 打开转换开关后，路由组内 WorkBuddy 成员按组内优先级参与 responses 路由，
// 并渲染回 responses 形态。
func TestResponsesConvertSwitchRoutesToWorkBuddy(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, wbSSEFixture)
	})
	nativeHit := false
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHit = true
		fmt.Fprint(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer native.Close()

	s, app := newTestServer(t)
	s.store.SetResponsesConvert(true)
	addWBProviderModel(t, s, up.URL, "shared", &WorkBuddyCred{UID: "u", AccessToken: "at-1"}) // 优先级 10
	addProviderModel(t, s, native.URL, "shared", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/responses", `{"model":"auto","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if nativeHit {
		t.Fatal("开启转换后应优先使用高优先级的 WorkBuddy 成员")
	}
	if !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, "你好") {
		t.Fatalf("应把 chat 流渲染为 response 对象: %s", body)
	}
}

// 设置接口的两个字段互相独立：只改其中一项不会把另一项重置。

func TestSettingsResponsesConvert(t *testing.T) {
	_, app := newTestServer(t)
	resp, body := postJSONWithHeader(t, app, "PUT", "/api/settings",
		`{"responses_convert":true}`, "Content-Type", "application/json")
	if resp.StatusCode != 200 {
		t.Fatalf("settings 应 200: %d %s", resp.StatusCode, body)
	}
	var v struct {
		AutoModel        string `json:"auto_model"`
		ResponsesConvert bool   `json:"responses_convert"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if !v.ResponsesConvert {
		t.Fatalf("responses_convert 应为 true: %s", body)
	}

	resp, body = postJSONWithHeader(t, app, "PUT", "/api/settings",
		`{"auto_model":"pick"}`, "Content-Type", "application/json")
	if resp.StatusCode != 200 {
		t.Fatalf("settings 应 200: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.AutoModel != "pick" || !v.ResponsesConvert {
		t.Fatalf("只改 auto_model 不应影响转换开关: %s", body)
	}
}
