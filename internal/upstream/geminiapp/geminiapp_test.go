package geminiapp

// 端到端测试：假 Robin 服务（TLS + HTTP/2，走真实的 HTTP/2 分帧与尾部帧路径）、
// 假 OAuth 端点与假图片上传服务，直接驱动适配器。全部为合成数据，不连 Google。

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/store"
	"llmrouter/internal/upstream"
)

// deltaOr chat 分片 delta 里的字符串字段
func deltaOr(delta map[string]any, key string) string {
	s, _ := delta[key].(string)
	return s
}

type uploadRecord struct {
	method, path, auth, pushID, command string
	bodyLen                             int
}

// fakeGemini 一组假上游
type fakeGemini struct {
	t       *testing.T
	robin   *httptest.Server
	oauth   *httptest.Server
	uploads *httptest.Server
	store   *store.Store
	a       *Adapter

	mu        sync.Mutex
	scripts   []func(http.ResponseWriter) // ProcessQuery 请求依次取用；只剩一个时反复使用
	gotBodies [][]byte
	gotAuth   []string
	gotHeader []http.Header
	tokenForm []url.Values
	revoked   []url.Values
	userinfos []string
	uploadLog []uploadRecord
	uploadURL string // 非空：上传服务返回这个「上传地址」（测试同源校验用）
}

func newFakeGemini(t *testing.T) *fakeGemini {
	t.Helper()
	t.Setenv(oauthClientIDEnv, "test-client-id")
	t.Setenv(oauthClientSecretEnv, "test-client-secret")
	f := &fakeGemini{t: t}
	f.store, _ = store.LoadStore(filepath.Join(t.TempDir(), "data.json"))
	robin := httptest.NewUnstartedServer(http.HandlerFunc(f.serveRobin))
	robin.EnableHTTP2 = true
	robin.StartTLS()
	f.robin = robin
	f.oauth = httptest.NewServer(http.HandlerFunc(f.serveOAuth))
	f.uploads = httptest.NewServer(http.HandlerFunc(f.serveUploads))
	t.Cleanup(func() {
		robin.Close()
		f.oauth.Close()
		f.uploads.Close()
	})
	rt := &upstream.Runtime{Store: f.store, Clients: upstream.NewClientFactory(robin.Client())}
	f.a = NewWithBases(rt, f.oauth.URL, f.robin.URL, f.uploads.URL)
	return f
}

// script 追加脚本：同一场景内多次 ProcessQuery 请求依次取用，只剩一个时反复使用
func (f *fakeGemini) script(s ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, s...)
}

// setScript 换一个场景：丢弃上一场景还没用到的脚本
func (f *fakeGemini) setScript(s ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = s
}

func (f *fakeGemini) serveRobin(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case processQueryPath:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.gotBodies = append(f.gotBodies, body)
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		f.gotHeader = append(f.gotHeader, r.Header.Clone())
		script := f.scripts[0]
		if len(f.scripts) > 1 {
			f.scripts = f.scripts[1:]
		}
		f.mu.Unlock()
		script(w)
	case usageInfoPath:
		writeGrpc(w, concat(pbVarint(2, 2), // AI Pro
			pbBytes(3, pbBytes(2, concat(
				pbBytes(1, concat(pbVarint(1, 1700000000), pbVarint(2, 0))),
				pbFixed32(2, 0.25), pbString(3, "Gemini 4 Pro"))))))
	case termsVersionPath:
		writeGrpc(w, concat(pbVarint(1, 7), pbVarint(2, 42)))
	case termsAcceptPath:
		writeGrpc(w, pbBytes(1, []byte("ok")))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGemini) serveOAuth(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenForm = append(f.tokenForm, r.PostForm)
		dead := r.PostForm.Get("refresh_token") == "rt-dead"
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case dead:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
		case r.PostForm.Get("grant_type") == "authorization_code":
			io.WriteString(w, `{"access_token":"tok-login","refresh_token":"rt-new","expires_in":3600}`)
		default:
			io.WriteString(w, `{"access_token":"tok-refreshed","expires_in":3600}`)
		}
	case "/userinfo":
		f.mu.Lock()
		f.userinfos = append(f.userinfos, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"sub":"sub-1","email":"someone@example.com","email_verified":true}`)
	case "/revoke":
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGemini) serveUploads(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.uploadLog = append(f.uploadLog, uploadRecord{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"),
		r.Header.Get("Push-Id"), r.Header.Get("X-Goog-Upload-Command"), len(body)})
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/upload/":
		f.mu.Lock()
		location := f.uploadURL // 非空：模拟上游被诱导返回外部上传地址
		f.mu.Unlock()
		if location == "" {
			location = f.uploads.URL + "/upload/abc123?upload_id=X"
		}
		w.Header().Set("X-Goog-Upload-Status", "active")
		w.Header().Set("X-Goog-Upload-Url", location)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/"):
		w.Header().Set("X-Goog-Upload-Status", "final")
		io.WriteString(w, "blob-XYZ")
	default:
		http.NotFound(w, r)
	}
}

// pbFixed32 32 位浮点字段（线类型 5）
func pbFixed32(num int, v float32) []byte {
	out := pbPutVarint(nil, uint64(num)<<3|5)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	return append(out, b[:]...)
}

// writeGrpc 一元 gRPC 响应：单条消息 + 尾部 grpc-status 0
func writeGrpc(w http.ResponseWriter, msg []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Trailer", "Grpc-Status")
	w.WriteHeader(http.StatusOK)
	w.Write(grpcFrame(msg))
	w.Header().Set("Grpc-Status", "0")
}

// frameText 一帧生成事件：整文快照 + 档位 / 归属 / 完成标记
func frameText(text string, mode, thinking int, attribution string, completed bool) []byte {
	body := pbBytes(1, pbBytes(2, pbString(1, text)))
	if completed {
		body = append(body, pbVarint(9, 1)...)
	}
	body = append(body, pbVarint(11, 0)...)
	if attribution != "" {
		body = append(body, pbString(30, attribution)...)
	}
	body = concat(body, pbVarint(31, 0), pbVarint(41, uint64(mode)), pbVarint(42, uint64(thinking)))
	return grpcFrame(pbBytes(1003, pbBytes(1, body)))
}

// scriptStream 按顺序写出若干帧后正常关流
func scriptStream(frames ...[]byte) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		h := w.Header()
		h.Set("Content-Type", "application/grpc")
		h.Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(http.StatusOK)
		for _, fr := range frames {
			w.Write(fr)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		h.Set("Grpc-Status", "0")
	}
}

// scriptAbort 写出若干帧后异常中断（RST_STREAM）
func scriptAbort(frames ...[]byte) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		for _, fr := range frames {
			w.Write(fr)
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}
}

// addProvider 建一个已登录的 Gemini App Provider 与 neon 模型条目
func (f *fakeGemini) addProvider(mut func(*domain.GeminiAppCred)) (domain.Provider, domain.Model) {
	f.t.Helper()
	exp := time.Now().Add(time.Hour)
	cred := &domain.GeminiAppCred{Subject: "sub-1", Email: "someone@example.com", AccessToken: "tok-1", RefreshToken: "rt-1", ExpiresAt: &exp}
	if mut != nil {
		mut(cred)
	}
	p := &domain.Provider{Kind: Kind, Name: "gem", BaseURL: f.robin.URL + processQueryPath, GeminiApp: cred}
	f.store.UpsertProvider(p)
	m := &domain.Model{Name: "gemini-neon", ProviderID: p.ID, Expose: true}
	f.store.UpsertModel(m)
	return *p, *m
}

func (f *fakeGemini) attempt(p domain.Provider, m domain.Model, body string, responses bool) (*httptest.ResponseRecorder, *domain.CallSample, error) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	proto := domain.ProtocolChat
	if responses {
		proto = domain.ProtocolResponses
	}
	sm := &domain.CallSample{}
	err := f.a.Attempt(&upstream.Attempt{Writer: rec, Request: req, Candidate: domain.Candidate{Provider: p, Model: m},
		Body: []byte(body), Protocol: proto, Timing: upstream.NewTiming(), Usage: sm})
	return rec, sm, err
}

// sseData 解析 SSE：每条 data 载荷解析成对象，[DONE] 记为 nil
func sseData(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			out = append(out, nil)
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("SSE 载荷不是 JSON: %q", payload)
		}
		out = append(out, v)
	}
	return out
}

func sseEvents(body string) []string { // SSE 的事件名（event: 行）
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	return out
}

const neon = "Used 4 Neon High model"

// ── 四种输出模式 ────────────────────────────────────────────────────────────

func TestAttemptChatStream(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("Hello", 3, 2, neon, false), frameText("Hello, 世界!\nDone.", 3, 2, neon, true)))
	rec, sm, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"stream":true}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("状态/类型不对: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	data := sseData(t, rec.Body.String())
	if len(data) != 4 {
		t.Fatalf("应有 4 条 data（role / 正文 / 终片 / [DONE]），got %d:\n%s", len(data), rec.Body)
	}
	if delta := data[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any); delta["role"] != "assistant" {
		t.Errorf("首片应为 role 分片: %v", data[0])
	}
	// 提交前扣住的正文在提交时合并成一条增量
	if delta := data[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any); delta["content"] != "Hello, 世界!\nDone." {
		t.Errorf("正文增量不对: %v", data[1])
	}
	finish := data[2]["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if finish != "stop" {
		t.Errorf("finish_reason = %v", finish)
	}
	if data[3] != nil {
		t.Errorf("应以 [DONE] 结束: %v", data[3])
	}
	if sm.Prompt <= 0 || sm.Completion <= 0 {
		t.Errorf("应记下估算用量: %+v", sm.Usage)
	}
	if sm.EffortSent != "thinking=2" {
		t.Errorf("实发思考档位: %q", sm.EffortSent)
	}
	// 出站请求：三条 gRPC 帧，头对齐线协议
	if len(f.gotBodies) != 1 || f.gotAuth[0] != "Bearer tok-1" {
		t.Fatalf("上游应只收到 1 次请求并带 Bearer: %d %v", len(f.gotBodies), f.gotAuth)
	}
	h := f.gotHeader[0]
	for k, want := range map[string]string{"Content-Type": "application/grpc", "Te": "trailers", "User-Agent": gatewayUserAgent} {
		if got := h.Get(k); got != want {
			t.Errorf("请求头 %s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasSuffix(h.Get("Grpc-Timeout"), "m") || !strings.HasPrefix(h.Get("Grpc-Timeout"), "180000") {
		t.Errorf("grpc-timeout = %q", h.Get("Grpc-Timeout"))
	}
	if got := countFrames(f.gotBodies[0]); got != 3 {
		t.Errorf("请求体应有 3 条 gRPC 帧, got %d", got)
	}
	if !strings.Contains(string(f.gotBodies[0]), "[System Instructions]") || !strings.Contains(string(f.gotBodies[0]), "[User]\nhi") {
		t.Errorf("出站提示词应含系统指令与对话: %q", truncate(f.gotBodies[0], 300))
	}
}

// 首段提交后继续到达的正文必须逐段下发，不能留在缓冲里丢掉
func TestAttemptChatStreamBeyondPrefix(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	prefix := strings.Repeat("0123456789abcdef", 200) + "\n" // 3200 字节，超过提交门槛
	f.script(scriptStream(frameText(prefix, 3, 2, neon, false), frameText(prefix+"TAIL", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"stream":true}`, false)
	if err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for _, d := range sseData(t, rec.Body.String()) {
		if d == nil {
			continue
		}
		choice := d["choices"].([]any)[0].(map[string]any)
		content.WriteString(deltaOr(choice["delta"].(map[string]any), "content"))
	}
	if content.String() != prefix+"TAIL" {
		t.Fatalf("首段提交后的后续正文丢失: got %d bytes, want %d", content.Len(), len(prefix)+4)
	}
}

func countFrames(b []byte) int {
	n := 0
	for len(b) >= 5 {
		size := int(binary.BigEndian.Uint32(b[1:5]))
		b = b[5+size:]
		n++
	}
	return n
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

func TestAttemptChatNonStream(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("Hello, 世界!\nDone.", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || v["object"] != "chat.completion" {
		t.Fatalf("应聚合为 chat.completion: %d %s", rec.Code, rec.Body)
	}
	choice := v["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello, 世界!\nDone." || msg["role"] != "assistant" || choice["finish_reason"] != "stop" {
		t.Errorf("聚合结果不对: %s", rec.Body)
	}
	if usage := v["usage"].(map[string]any); usage["estimated"] != true || usage["prompt_tokens"].(float64) <= 0 {
		t.Errorf("用量应标注为估算值: %v", usage)
	}
}

func TestAttemptResponsesStream(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("Hi", 3, 2, neon, false), frameText("Hi there.", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","input":"hello","stream":true}`, true)
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
	if got := sseEvents(body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列不对:\n got %v\nwant %v\n%s", got, want, body)
	}
	for _, want := range []string{`"delta":"Hi there."`, `"text":"Hi there."`, `"status":"completed"`, `"sequence_number":8`, `"annotations":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q:\n%s", want, body)
		}
	}
}

func TestAttemptResponsesNonStream(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("Hi there.", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","input":"hello"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			Estimated    bool  `json:"estimated"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Object != "response" || v.Status != "completed" || len(v.Output) != 1 || v.Output[0].Content[0].Text != "Hi there." {
		t.Fatalf("response 对象不对: %s", rec.Body)
	}
	if !v.Usage.Estimated || v.Usage.OutputTokens <= 0 {
		t.Errorf("usage 不对: %+v", v.Usage)
	}
}

// ── 工具调用 ────────────────────────────────────────────────────────────────

const toolBody = `{"model":"gemini-neon","messages":[` +
	`{"role":"user","content":"what time is it"}],` +
	`"tools":[{"type":"function","function":{"name":"get_time","description":"Get the time","parameters":{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}}}]` +
	`,"tool_choice":"auto"}`

const toolReply = "Let me check.\n```tool_call\n{\"name\":\"get_time\",\"arguments\":{\"tz\":\"UTC\"}}\n```"

func TestAttemptToolCallStream(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("Let me check.\n", 3, 2, neon, false), frameText(toolReply, 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, strings.Replace(toolBody, `"tool_choice":"auto"`, `"tool_choice":"auto","stream":true`, 1), false)
	if err != nil {
		t.Fatal(err)
	}
	data := sseData(t, rec.Body.String())
	var content strings.Builder
	var calls []map[string]any
	finish := ""
	for _, d := range data[:len(data)-1] {
		choice := d["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		content.WriteString(deltaOr(delta, "content"))
		if tc, ok := delta["tool_calls"].([]any); ok {
			calls = append(calls, tc[0].(map[string]any))
		}
		if fr, ok := choice["finish_reason"].(string); ok {
			finish = fr
		}
	}
	if content.String() != "Let me check.\n" || len(calls) != 1 || finish != "tool_calls" {
		t.Fatalf("工具调用流不对: content=%q calls=%v finish=%q\n%s", content.String(), calls, finish, rec.Body)
	}
	call := calls[0]
	fn := call["function"].(map[string]any)
	if !strings.HasPrefix(call["id"].(string), "call_") || fn["name"] != "get_time" || fn["arguments"] != `{"tz":"UTC"}` {
		t.Errorf("tool_call 形态不对: %v", call)
	}
	// 工具协议说明应进入出站提示词
	if out := string(f.gotBodies[0]); !strings.Contains(out, "```tool_call") || !strings.Contains(out, `"name":"get_time"`) {
		t.Errorf("出站提示词应含工具声明与协议说明")
	}
}

func TestAttemptToolCallResponses(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText(toolReply, 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","input":"what time is it","tools":[{"type":"function","name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}}]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Output) != 2 || v.Output[1].Type != "function_call" || v.Output[1].Name != "get_time" || v.Output[1].Arguments != `{"tz":"UTC"}` || !strings.HasPrefix(v.Output[1].CallID, "call_") {
		t.Fatalf("function_call 输出不对: %s", rec.Body)
	}
}

// 工具调用先发出、正文后到时，Responses 的 output_index 不能漂移
func TestAttemptResponsesToolThenTextIndexes(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	text := "```tool_call\n{\"name\":\"get_time\",\"arguments\":{\"tz\":\"UTC\"}}\n```\nAfter."
	f.script(scriptStream(frameText(text, 3, 2, neon, true)))
	body := `{"model":"gemini-neon","input":"what time is it","stream":true,` +
		`"tools":[{"type":"function","name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}}]}`
	rec, _, err := f.attempt(p, m, body, true)
	if err != nil {
		t.Fatal(err)
	}
	var added []int
	var outputTypes []string
	var messageText string
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		data := ""
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		if strings.Contains(block, "event: response.output_item.added") {
			var v struct {
				OutputIndex int `json:"output_index"`
			}
			if err := json.Unmarshal([]byte(data), &v); err != nil {
				t.Fatalf("output_item.added 不是 JSON: %v\n%s", err, block)
			}
			added = append(added, v.OutputIndex)
		}
		if strings.Contains(block, "event: response.completed") {
			var v struct {
				Response struct {
					Output []struct {
						Type    string `json:"type"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &v); err != nil {
				t.Fatalf("response.completed 不是 JSON: %v\n%s", err, block)
			}
			for _, it := range v.Response.Output {
				outputTypes = append(outputTypes, it.Type)
				if it.Type == "message" && len(it.Content) > 0 {
					messageText = it.Content[0].Text
				}
			}
		}
	}
	if len(added) != 2 || added[0] != 0 || added[1] != 1 {
		t.Fatalf("output_index 应稳定为 0/1，got %v\n%s", added, rec.Body.String())
	}
	if strings.Join(outputTypes, ",") != "message,function_call" {
		t.Errorf("最终输出应为 message → function_call，got %v", outputTypes)
	}
	if !strings.Contains(messageText, "After.") {
		t.Errorf("工具调用后的正文应完整交付，got %q", messageText)
	}
}

func TestAttemptTruncatedToolCallNotReleased(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	// 流在工具围栏闭合前结束：残缺的调用不能下发给客户端执行
	f.script(scriptStream(frameText("Let me check.\n```tool_call\n{\"name\":\"get_time\",\"arguments\":{\"tz\":\"UTC\"}}\n", 3, 2, neon, true)))
	_, _, err := f.attempt(p, m, toolBody, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 502 || !strings.Contains(string(se.Body), "TRUNCATED_TOOL_CALL") {
		t.Fatalf("残缺工具调用应报 502 TRUNCATED_TOOL_CALL, got %v", err)
	}
}

// 模型请求了未声明的工具：拒绝整个回合
func TestAttemptUndeclaredToolRejected(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("```tool_call\n{\"name\":\"rm_rf\",\"arguments\":{}}\n```", 3, 2, neon, true)))
	_, _, err := f.attempt(p, m, toolBody, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 502 || !strings.Contains(string(se.Body), "UNDECLARED_TOOL") {
		t.Fatalf("未声明工具应报 502 UNDECLARED_TOOL, got %v", err)
	}
}

// ── 鉴权 / 限流 / 降级 ──────────────────────────────────────────────────────

func TestAttemptRefreshesOn401(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(func(w http.ResponseWriter) { // 首次：token 已失效
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "16")
		w.Header().Set("Grpc-Message", "invalid%20token")
		w.WriteHeader(http.StatusUnauthorized)
	}, scriptStream(frameText("ok", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.gotAuth) != 2 || f.gotAuth[0] != "Bearer tok-1" || f.gotAuth[1] != "Bearer tok-refreshed" {
		t.Fatalf("应先用旧 token、续期后用新 token 重试: %v", f.gotAuth)
	}
	if len(f.tokenForm) != 1 || f.tokenForm[0].Get("grant_type") != "refresh_token" || f.tokenForm[0].Get("refresh_token") != "rt-1" ||
		!strings.Contains(f.tokenForm[0].Get("scope"), "userinfo.email") || f.tokenForm[0].Get("client_id") != f.a.oauthClientID {
		t.Fatalf("续期表单不对: %v", f.tokenForm)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("重试成功后应返回内容: %s", rec.Body)
	}
	// 续期结果落盘
	saved, _ := f.store.Provider(p.ID)
	if saved.GeminiApp.AccessToken != "tok-refreshed" || saved.GeminiApp.Expires().Before(time.Now()) {
		t.Errorf("续期后的 token 应落盘: %+v", saved.GeminiApp)
	}
}

func TestAttemptRevokedRefreshToken(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(func(c *domain.GeminiAppCred) {
		c.RefreshToken = "rt-dead"
		c.AccessToken, c.ExpiresAt = "", nil // 没有可用 token，必须续期
	})
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("授权失效应返回 401 StatusError, got %v", err)
	}
	saved, _ := f.store.Provider(p.ID)
	if saved.GeminiApp.AuthError == "" {
		t.Errorf("应记下授权失效，供后台提示重登: %+v", saved.GeminiApp)
	}
}

func TestAttemptQuotaMuted(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "8")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 429 {
		t.Fatalf("应返回 429, got %v", err)
	}
	saved, _ := f.store.Model(m.ID)
	if saved.MutedUntil.Before(time.Now()) {
		t.Errorf("429 后模型条目应被自动静音: %+v", saved)
	}
	// X
	if remaining := time.Until(saved.MutedUntil); remaining > 90*time.Second {
		t.Errorf("瞬时限流应短静音, got %v", remaining)
	}
}

func TestAttemptQuotaDowngrade(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	// 请求 Neon，上游因额度不足降级成 Hydrogen：按 429 处理并长静音
	body := pbBytes(1, pbBytes(2, pbString(1, "hi")))
	frame := grpcFrame(pbBytes(1003, pbBytes(1, concat(body, pbVarint(9, 1), pbString(30, "Used 4 Hydrogen High model"),
		pbVarint(31, 2), pbVarint(41, 1), pbVarint(42, 2)))))
	f.script(scriptStream(frame))
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 429 || !strings.Contains(string(se.Body), "NATIVE_QUOTA_EXHAUSTED") {
		t.Fatalf("额度降级应按 429 处理, got %v", err)
	}
	saved, _ := f.store.Model(m.ID)
	if time.Until(saved.MutedUntil) < 4*time.Hour {
		t.Errorf("套餐额度耗尽应长静音（5 小时）, got %v", time.Until(saved.MutedUntil))
	}
}

// 已交付足量正文后才收到额度降级：仍按 429 长静音，但客户端拿到 incomplete
func TestAttemptQuotaDowngradeAfterOutputMutes(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	long := strings.Repeat("0123456789abcdef", 200) + "\n" // 3200 字节，超过提交门槛
	first := frameText(long, 3, 2, neon, false)
	// 第二帧：同样的整文快照 + 完成标记 + 额度降级原因
	body := pbBytes(1, pbBytes(2, pbString(1, long)))
	body = concat(body, pbVarint(9, 1), pbString(30, neon), pbVarint(31, 2), pbVarint(41, 3), pbVarint(42, 2))
	second := grpcFrame(pbBytes(1003, pbBytes(1, body)))
	f.script(scriptStream(first, second))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"stream":true}`, false)
	if err != nil {
		t.Fatalf("已交付内容后中断应平稳收尾: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"incomplete":true`) {
		t.Errorf("终片应标记 incomplete:\n%s", rec.Body.String())
	}
	saved, _ := f.store.Model(m.ID)
	if time.Until(saved.MutedUntil) < 4*time.Hour {
		t.Errorf("已交付内容后的额度降级仍应长静音, got %v", time.Until(saved.MutedUntil))
	}
}

func TestAttemptModelDowngradeRejected(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("hi", 1, 2, "Used 4 Hydrogen High model", true))) // 要 Neon 给了 Hydrogen
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 502 || !strings.Contains(string(se.Body), "MODEL_DOWNGRADE_REJECTED") {
		t.Fatalf("降级响应应被拒绝, got %v", err)
	}
}

// ── 中断收尾 ────────────────────────────────────────────────────────────────

// 已交付足量正文后上游断流：平稳收尾并标记 incomplete，不回错误
func TestAttemptInterruptedAfterUsableOutput(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	long := strings.Repeat("0123456789abcdef", 200) + "\n" // 3200 字节，超过提交门槛
	f.script(scriptAbort(frameText(long, 3, 2, neon, false)))
	rec, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"stream":true}`, false)
	if err != nil {
		t.Fatal(err)
	}
	data := sseData(t, rec.Body.String())
	if len(data) < 3 {
		t.Fatalf("应有正文、终片与 [DONE]:\n%s", rec.Body)
	}
	final := data[len(data)-2]
	if final["incomplete"] != true || final["incomplete_details"].(map[string]any)["reason"] != "upstream_interrupted" {
		t.Errorf("终片应标记 incomplete: %v", final)
	}
	if fr := final["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
		t.Errorf("finish_reason = %v", fr)
	}
	if data[len(data)-1] != nil {
		t.Errorf("应以 [DONE] 结束")
	}
}

// 不足门槛的残片不算可用：按失败处理（响应头尚未提交，可换候选）
func TestAttemptInterruptedBeforeUsableOutput(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptAbort(frameText("short", 3, 2, neon, false)))
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"stream":true}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("未提交流时应返回错误交还候选循环, got %v", err)
	}
}

// ── 图片 ────────────────────────────────────────────────────────────────────

func tinyImage(t *testing.T, mime string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	var err error
	switch mime {
	case "image/png":
		err = png.Encode(&buf, img)
	case "image/jpeg":
		err = jpeg.Encode(&buf, img, nil)
	default:
		t.Fatalf("unknown mime %s", mime)
	}
	if err != nil {
		t.Fatal(err)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestAttemptImageUpload(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("A red square.", 3, 2, neon, true)))
	dataURL := tinyImage(t, "image/png")
	body, _ := json.Marshal(map[string]any{"model": "gemini-neon", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
	}}}})
	rec, _, err := f.attempt(p, m, string(body), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "A red square.") {
		t.Fatalf("应返回内容: %s", rec.Body)
	}
	log := f.uploadLog
	if len(log) != 2 || log[0].method != "POST" || log[0].command != "start" || log[0].auth != "Bearer tok-1" || log[0].pushID != "feeds/mcudyrk2a4khkz" ||
		log[1].method != "PUT" || log[1].command != "upload, finalize" || log[1].auth != "" || log[1].bodyLen <= 0 {
		t.Fatalf("上传流程不对: %+v", log)
	}
	out := string(f.gotBodies[0])
	if !strings.Contains(out, "blob-XYZ") || !strings.Contains(out, "image-1.png") || !strings.Contains(out, "[Attached user image 1: image-1.png]") {
		t.Errorf("回合请求应引用上传得到的 Blob ID 并留占位标记")
	}
}

func TestRemoteImageURLRejected(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	body := `{"model":"gemini-neon","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`
	_, _, err := f.attempt(p, m, body, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 400 {
		t.Fatalf("远程图片 URL 应 400（不代为下载）, got %v", err)
	}
	if len(f.gotBodies) != 0 || len(f.uploadLog) != 0 {
		t.Errorf("不应触达上游")
	}
}

// ── 请求归一化 ──────────────────────────────────────────────────────────────

func TestBuildPlanRejectsUnsupported(t *testing.T) {
	for name, body := range map[string]string{
		"未知模型":          `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`,
		"native_search": `{"model":"gemini-neon","native_search":true,"messages":[{"role":"user","content":"hi"}]}`,
		"Deep Think":    `{"model":"gemini-neon","thinking_level":3,"messages":[{"role":"user","content":"hi"}]}`,
		"缺 messages":    `{"model":"gemini-neon"}`,
	} {
		_, err := buildPlan([]byte(body), false)
		te := &turnError{}
		if !errors.As(err, &te) || te.Status != 400 {
			t.Errorf("%s: 应 400, got %v", name, err)
		}
	}
}

func TestBuildPlanAliasAndEffort(t *testing.T) {
	plan, err := buildPlan([]byte(`{"model":"gemini-argon","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.model != "gemini-neon" || plan.thinkingLevel != 1 {
		t.Errorf("别名与档位映射不对: %s %d", plan.model, plan.thinkingLevel)
	}
	// responses 入口：嵌套的 reasoning.effort 优先；未知字段忽略
	plan, err = buildPlan([]byte(`{"model":"gemini-hydrogen","input":"hi","reasoning":{"effort":"none","summary":"auto"},"store":false,"prompt_cache_key":"k"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.model != "gemini-hydrogen" || plan.thinkingLevel != 0 {
		t.Errorf("responses 档位映射不对: %s %d", plan.model, plan.thinkingLevel)
	}
	// 关闭工具：围栏只是普通文本，不解析
	plan, err = buildPlan([]byte(`{"model":"gemini-neon","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"none"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.policy.enabled() || strings.Contains(plan.prompt, "Available Local Tools") {
		t.Errorf("tool_choice=none 时不应启用工具协议")
	}
}

func TestStructuredOutputValidated(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	body := `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	f.script(scriptStream(frameText("```json\n{\"a\":1}\n```", 3, 2, neon, true)))
	rec, _, err := f.attempt(p, m, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"{\"a\":1}"`) {
		t.Errorf("应去掉唯一的顶层围栏后交付: %s", rec.Body)
	}
	// 不是 JSON：不交付
	f.setScript(scriptStream(frameText("Sorry, I cannot do that.", 3, 2, neon, true)))
	_, _, err = f.attempt(p, m, body, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || !strings.Contains(string(se.Body), "INVALID_STRUCTURED_OUTPUT_JSON") {
		t.Fatalf("非 JSON 输出应被拒绝, got %v", err)
	}
}

func TestEmptyResponseRejected(t *testing.T) {
	f := newFakeGemini(t)
	p, m := f.addProvider(nil)
	f.script(scriptStream(frameText("   ", 3, 2, neon, true)))
	_, _, err := f.attempt(p, m, `{"model":"gemini-neon","messages":[{"role":"user","content":"hi"}]}`, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || !strings.Contains(string(se.Body), "EMPTY_NATIVE_RESPONSE") {
		t.Fatalf("空响应应报错, got %v", err)
	}
}
