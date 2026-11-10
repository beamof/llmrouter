package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ccTestServer 起一个假的 Command Code 上游，并返回指向它的 llmrouter。
func ccTestServer(t *testing.T, generate http.HandlerFunc) (*Server, *httptest.Server, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /alpha/generate", generate)
	mux.HandleFunc("GET /alpha/whoami", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"user":{"id":"u1","userName":"alice","email":"a@example.com"}}`)
	})
	mux.HandleFunc("GET /alpha/billing/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"planId":"individual-go-monthly","status":"active","currentPeriodEnd":1790000000000}}`)
	})
	mux.HandleFunc("GET /provider/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"deepseek/deepseek-v4-pro"},{"id":"claude-opus-5-5"},{"id":"gpt-6-sol"},{"id":"gpt-6-luna"}]}`)
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	st, err := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, client: &http.Client{}, ccBase: up.URL}
	app := httptest.NewServer(s.routes())
	t.Cleanup(app.Close)
	return s, app, up
}

func addCCProviderModel(t *testing.T, s *Server, model string) *Model {
	t.Helper()
	p := &Provider{
		Name: "cc", Kind: commandCodeKind, BaseURL: s.ccAPI() + "/alpha/generate",
		CommandCode: &CommandCodeCred{APIKey: "cc-key", Plan: "Go"},
	}
	s.store.UpsertProvider(p)
	m := &Model{Name: model, ProviderID: p.ID, Priority: 10}
	s.store.UpsertModel(m)
	return m
}

func TestCommandCodeChatNonStream(t *testing.T) {
	var gotBody string
	var gotHeader http.Header
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHeader = string(b), r.Header.Clone()
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			`{"type":"reasoning-delta","text":"想"}`+"\n",
			`{"type":"text-delta","text":"你好"}`+"\n",
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":3,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":1},"outputTokenDetails":{"reasoningTokens":1}}}`+"\n")
	})
	addCCProviderModel(t, s, "deepseek/deepseek-v4-pro")

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"deepseek/deepseek-v4-pro","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_tokens":500000,"stream":false}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			Details          struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", body)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "你好" || out.Choices[0].Message.ReasoningContent != "想" {
		t.Fatalf("choices = %+v", out.Choices)
	}
	if out.Choices[0].FinishReason != "stop" || out.Usage.PromptTokens != 3 || out.Usage.CompletionTokens != 2 || out.Usage.Details.Cached != 1 {
		t.Fatalf("finish/usage = %+v / %+v", out.Choices[0].FinishReason, out.Usage)
	}

	// 出站信封：system、messages、tools、max_tokens 上限、stream 恒 true
	var req struct {
		Config struct {
			WorkingDir  string `json:"workingDir"`
			Environment string `json:"environment"`
		} `json:"config"`
		Params struct {
			Model      string           `json:"model"`
			Messages   []map[string]any `json:"messages"`
			System     string           `json:"system"`
			Tools      []any            `json:"tools"`
			MaxTokens  int64            `json:"max_tokens"`
			Stream     bool             `json:"stream"`
			Permission string           `json:"permissionMode"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("出站不是 JSON: %s", gotBody)
	}
	if req.Params.Model != "deepseek/deepseek-v4-pro" || req.Params.System != "sys" || !req.Params.Stream {
		t.Fatalf("params = %+v", req.Params)
	}
	if req.Params.MaxTokens != commandCodeMaxOutput {
		t.Errorf("max_tokens = %d, want %d", req.Params.MaxTokens, commandCodeMaxOutput)
	}
	if len(req.Params.Messages) != 1 || req.Params.Messages[0]["role"] != "user" {
		t.Errorf("messages = %+v", req.Params.Messages)
	}
	if req.Config.WorkingDir == "" || req.Config.Environment == "" {
		t.Errorf("config = %+v", req.Config)
	}
	if gotHeader.Get("Authorization") != "Bearer cc-key" || gotHeader.Get("User-Agent") != "cli" ||
		gotHeader.Get("x-command-code-version") != commandCodeCLIVersion || gotHeader.Get("x-session-id") == "" {
		t.Errorf("headers = %+v", gotHeader)
	}

	var state struct {
		Usage []UsageStat `json:"usage"`
	}
	json.Unmarshal(s.store.SnapshotJSON(), &state)
	if len(state.Usage) != 1 || state.Usage[0].PromptTokens != 3 || state.Usage[0].CompletionTokens != 2 {
		t.Errorf("usage = %+v", state.Usage)
	}
}

func TestCommandCodeChatStreaming(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			`{"type":"text-delta","text":"你"}`+"\n",
			`{"type":"text-delta","text":"好"}`+"\n",
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":2}}}`+"\n")
	})
	addCCProviderModel(t, s, "m")

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"role":"assistant"`, `"content":"你"`, `"content":"好"`, `"finish_reason":"stop"`, `"cached_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("流式响应缺少 %q:\n%s", want, body)
		}
	}
}

func TestCommandCodeResponses(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			`{"type":"text-delta","text":"答"}`+"\n",
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":4,"outputTokens":1}}`+"\n")
	})
	addCCProviderModel(t, s, "m")

	// 非流式：responses 对象
	resp, body := postJSON(t, app, "/v1/responses", `{"model":"m","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, `"output_text"`) || !strings.Contains(body, `"text":"答"`) {
		t.Errorf("responses 非流式 = %s", body)
	}

	// 流式：responses 事件流
	resp, body = postJSON(t, app, "/v1/responses", `{"model":"m","input":"hi","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("stream status = %d, body = %s", resp.StatusCode, body)
	}
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed", `"text":"答"`} {
		if !strings.Contains(body, want) {
			t.Errorf("responses 流缺少 %q:\n%s", want, body)
		}
	}
}

func TestCommandCodeToolCall(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			`{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{"path":"a.txt"}}`+"\n",
			`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":3,"outputTokens":4}}`+"\n")
	})
	addCCProviderModel(t, s, "m")

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"read"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"stream":false}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"read"`) ||
		!strings.Contains(body, `\"path\":\"a.txt\"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("tool call 响应 = %s", body)
	}
}

func TestCommandCodeCutShort(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"type":"text-delta","text":"半句"}`+"\n")
	})
	addCCProviderModel(t, s, "m")

	// 流式：已写出内容 → 补说明并收尾
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "半句") || !strings.Contains(body, "cut short") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("截断流 = %d %s", resp.StatusCode, body)
	}

	// 非流式：尚未提交 → 502
	resp, body = postJSON(t, app, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if resp.StatusCode != 502 || !strings.Contains(body, "ended before it was complete") {
		t.Errorf("截断非流式 = %d %s", resp.StatusCode, body)
	}
}

func TestCommandCode429Mutes(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"RATE_LIMITED: usage limit reached"}}`)
	})
	m := addCCProviderModel(t, s, "m")

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 502 || !strings.Contains(body, "429") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	got, _ := s.store.Model(m.ID)
	if got.MutedUntil.IsZero() {
		t.Fatalf("429 后模型应被静音: %+v", got)
	}
}

func TestCommandCodeImportFromCLI(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"apiKey":"cli-key","userId":"u1","userName":"alice"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := commandCodeAuthPath
	commandCodeAuthPath = func() string { return auth }
	t.Cleanup(func() { commandCodeAuthPath = old })

	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, body := postJSON(t, app, "/api/commandcode/import", `{"name":"CC"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("import = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Provider string `json:"provider"`
		Models   int    `json:"models"`
		Plan     string `json:"plan"`
		Warning  string `json:"warning"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.Provider == "" || out.Plan != "Go" || out.Models != 2 {
		t.Fatalf("import result = %+v (%s)", out, body)
	}
	p, ok := s.store.Provider(out.Provider)
	if !ok || p.Kind != commandCodeKind || p.CommandCode == nil || p.CommandCode.APIKey != "cli-key" ||
		!p.CommandCode.CLI || p.BaseURL != s.ccAPI()+"/alpha/generate" {
		t.Fatalf("provider = %+v", p)
	}
	// Go 套餐过滤掉 premium 模型，只留下 deepseek 与 gpt-6-luna
	var names []string
	for _, m := range s.store.Models {
		if m.ProviderID == p.ID {
			names = append(names, m.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("models = %v", names)
	}
}

func TestCommandCodeRefreshPicksUpCLIKey(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	os.WriteFile(auth, []byte(`{"apiKey":"old-key"}`), 0o600)
	old := commandCodeAuthPath
	commandCodeAuthPath = func() string { return auth }
	t.Cleanup(func() { commandCodeAuthPath = old })

	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	p := &Provider{
		Name: "cc", Kind: commandCodeKind, BaseURL: s.ccAPI() + "/alpha/generate",
		CommandCode: &CommandCodeCred{APIKey: "old-key", CLI: true},
	}
	s.store.UpsertProvider(p)

	os.WriteFile(auth, []byte(`{"apiKey":"new-key"}`), 0o600)
	resp, body := postJSON(t, app, "/api/providers/"+p.ID+"/commandcode-refresh", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("refresh = %d %s", resp.StatusCode, body)
	}
	got, _ := s.store.Provider(p.ID)
	if got.CommandCode == nil || got.CommandCode.APIKey != "new-key" || got.CommandCode.Plan != "Go" {
		t.Fatalf("cred = %+v", got.CommandCode)
	}
}

func TestCommandCodeImportReplacesKeyOnSameProvider(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	p := &Provider{
		Name: "cc", Kind: commandCodeKind, BaseURL: s.ccAPI() + "/alpha/generate",
		CommandCode: &CommandCodeCred{APIKey: "old-key"},
	}
	s.store.UpsertProvider(p)

	resp, body := postJSON(t, app, "/api/commandcode/import",
		`{"provider_id":"`+p.ID+`","name":"cc2","api_key":"new-key"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("import = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Provider string `json:"provider"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.Provider != p.ID {
		t.Fatalf("应原地更新同一个 provider: %+v", out)
	}
	got, _ := s.store.Provider(p.ID)
	if got.CommandCode == nil || got.CommandCode.APIKey != "new-key" || got.Name != "cc2" {
		t.Fatalf("provider = %+v", got)
	}
}

func TestCommandCodeUpstreamModelsAndTest(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	p := &Provider{
		Name: "cc", Kind: commandCodeKind, BaseURL: s.ccAPI() + "/alpha/generate",
		CommandCode: &CommandCodeCred{APIKey: "k", Plan: "Go"},
	}
	s.store.UpsertProvider(p)
	m := &Model{Name: "gpt-6-luna", ProviderID: p.ID}
	s.store.UpsertModel(m)

	resp, err := app.Client().Get(app.URL + "/api/providers/" + p.ID + "/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"gpt-6-luna"`) || strings.Contains(string(b), "claude-opus-5-5") {
		t.Errorf("catalog = %s", b)
	}

	resp2, body := postJSON(t, app, "/api/models/"+m.ID+"/test", `{}`)
	if resp2.StatusCode != 200 || !strings.Contains(body, `"level":"ok"`) {
		t.Errorf("model test = %d %s", resp2.StatusCode, body)
	}
}

func TestCommandCodeProviderBaseURLForced(t *testing.T) {
	s, app, _ := ccTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, body := postJSON(t, app, "/api/providers",
		`{"name":"cc","kind":"commandcode","base_url":"https://evil.example.com/x","commandcode":{"api_key":"k"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	ps := s.store.Providers
	if len(ps) != 1 || ps[0].BaseURL != s.ccAPI()+"/alpha/generate" {
		t.Fatalf("provider = %+v", ps)
	}
}

func TestCommandCodeBuildBodyFitsEffortAndGoogleSchema(t *testing.T) {
	body := []byte(`{"model":"google/gemini-3.6-flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"reasoning_effort":"max",
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","$defs":{"x":{"type":"object","properties":{"a":{"type":"string"}}}},"properties":{"v":{"$ref":"#/$defs/x/properties/a"}}}}}]}`)
	out, sentEffort, err := ccBuildBody(body, "google/gemini-3.6-flash")
	if err != nil {
		t.Fatal(err)
	}
	if sentEffort != "high" {
		t.Errorf("回传实发档位 = %q, want high", sentEffort)
	}
	var req struct {
		Params struct {
			ReasoningEffort string `json:"reasoning_effort"`
			Tools           []struct {
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
		} `json:"params"`
	}
	json.Unmarshal(out, &req)
	if req.Params.ReasoningEffort != "high" {
		t.Errorf("effort = %q, want high", req.Params.ReasoningEffort)
	}
	props, _ := req.Params.Tools[0].InputSchema["properties"].(map[string]any)
	if v, _ := props["v"].(map[string]any); v["$ref"] != nil {
		t.Errorf("Google 的嵌套 $ref 应被展开: %+v", props)
	}
}
