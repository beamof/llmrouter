package app

// Gemini App 上游经网关路由层的端到端测试：假 Robin 服务（TLS + HTTP/2），
// 覆盖代理入口、路由组故障转移与自动静音、直连错误透传、后台校验与管理接口。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llmrouter/internal/domain"
)

const (
	geminiProcessQuery = "/assistant.mobile.robin.RobinService/ProcessQuery"
	geminiUsageInfo    = "/assistant.mobile.robin.RobinService/GetUsageInfo"
	geminiTermsVersion = "/assistant.mobile.robin.RobinService/GetTermsOfServiceVersion"
	geminiTermsAccept  = "/assistant.mobile.robin.RobinService/AcceptTermsOfService"
)

// fakeRobin 假的 Gemini Robin 服务：按模式回复 ProcessQuery，并应答用量 / 条款 RPC
type fakeRobin struct {
	mu      sync.Mutex
	mode    string // ok | 429
	hits    int    // ProcessQuery 调用次数
	revoked []string
}

// ── 手写 protobuf / gRPC 分帧的测试小工具 ──

func frKey(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func frVarint(num int, v uint64) []byte { return append(frKey(uint64(num)<<3), frKey(v)...) }

func frBytes(num int, b []byte) []byte {
	return append(append(frKey(uint64(num)<<3|2), frKey(uint64(len(b)))...), b...)
}

func frFrame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// frText 一帧生成事件：整文快照（1.2.1）、完成标记（9）、归属（30）、档位（41 = Neon）
func frText(text string) []byte {
	event := frBytes(1, frBytes(2, frBytes(1, []byte(text))))
	event = append(event, frVarint(9, 1)...)
	event = append(event, frBytes(30, []byte("Used 4 Neon High model"))...)
	event = append(event, frVarint(41, 3)...)
	return frFrame(frBytes(1003, frBytes(1, event)))
}

// grpcOK 一元 gRPC 响应：单条消息 + 尾部 grpc-status 0
func grpcOK(w http.ResponseWriter, msg []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Trailer", "Grpc-Status")
	w.WriteHeader(http.StatusOK)
	w.Write(frFrame(msg))
	h.Set("Grpc-Status", "0")
}

func (f *fakeRobin) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	switch r.URL.Path {
	case geminiUsageInfo:
		grpcOK(w, frVarint(2, 2)) // 订阅档位：AI Pro
		return
	case geminiTermsVersion:
		grpcOK(w, frVarint(2, 42)) // 条款版本号
		return
	case geminiTermsAccept:
		grpcOK(w, frBytes(1, []byte("ok")))
		return
	case geminiProcessQuery: // 继续往下
	default:
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.hits++
	mode := f.mode
	f.mu.Unlock()
	h.Set("Content-Type", "application/grpc")
	h.Set("Trailer", "Grpc-Status")
	switch {
	case mode == "429":
		h.Set("Grpc-Status", "8")
		w.WriteHeader(http.StatusTooManyRequests)
	case r.Header.Get("Authorization") != "Bearer tok-ok" || r.Header.Get("Content-Type") != "application/grpc":
		h.Set("Grpc-Status", "16")
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusOK)
		w.Write(frText("Hello from Gemini"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		h.Set("Grpc-Status", "0")
	}
}

func (f *fakeRobin) setMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *fakeRobin) turns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *fakeRobin) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func newGeminiAppServer(t *testing.T) (*Server, *httptest.Server, *fakeRobin) {
	t.Helper()
	st, err := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRobin{mode: "ok"}
	robin := httptest.NewUnstartedServer(http.HandlerFunc(fr.serve))
	robin.EnableHTTP2 = true
	robin.StartTLS()
	t.Cleanup(robin.Close)
	// 假 OAuth 端点：续期 / 账号身份都走这里，测试不触达 Google
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"tok-ok","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"sub":"s1","email":"a@b.c"}`)
		case "/revoke":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fr.mu.Lock()
			fr.revoked = append(fr.revoked, r.PostForm.Get("token"))
			fr.mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(oauth.Close)
	// 共享客户端信任假上游的证书并能协商 HTTP/2（与生产的出站路径一致）
	s := &Server{store: st, client: robin.Client()}
	s.geminiOrigin, s.geminiOAuthBase = robin.URL, oauth.URL
	t.Setenv("LLMROUTER_GEMINI_OAUTH_PORT", "0") // 登录回环用随机端口（适配器首次使用时读取）
	t.Setenv("LLMROUTER_GEMINI_CLIENT_ID", "test-client-id")
	t.Setenv("LLMROUTER_GEMINI_CLIENT_SECRET", "test-client-secret")
	app := httptest.NewServer(s.routes())
	t.Cleanup(app.Close)
	return s, app, fr
}

func addGeminiProvider(t *testing.T, s *Server, priority int) (*Provider, *Model) {
	t.Helper()
	exp := time.Now().Add(time.Hour)
	cred := &domain.GeminiAppCred{Subject: "s1", Email: "a@b.c", AccessToken: "tok-ok", RefreshToken: "rt", ExpiresAt: &exp}
	p := &Provider{Kind: "geminiapp", Name: "gem", BaseURL: s.geminiOrigin + geminiProcessQuery, GeminiApp: cred}
	s.store.UpsertProvider(p)
	// 与登录时同步出来的模型条目一致：默认暴露，上下文声明取内置目录
	m := &Model{Name: "gemini-neon", ProviderID: p.ID, Priority: priority, Expose: true, ContextLength: 512000}
	s.store.UpsertModel(m)
	return p, m
}

func TestGeminiAppProxyRoutes(t *testing.T) {
	s, app, _ := newGeminiAppServer(t)
	p, _ := addGeminiProvider(t, s, 10)
	ask := `{"model":"gem/gemini-neon","messages":[{"role":"user","content":"hi"}]}`

	// chat 非流式：聚合为 chat.completion，用量标注为估算值
	resp, body := postJSON(t, app, "/v1/chat/completions", ask)
	if resp.StatusCode != 200 || !strings.Contains(body, `"content":"Hello from Gemini"`) || !strings.Contains(body, `"estimated":true`) {
		t.Fatalf("chat 非流式不对: %d %s", resp.StatusCode, body)
	}
	// chat 流式：SSE 分片 + [DONE]
	resp, body = postJSON(t, app, "/v1/chat/completions", strings.Replace(ask, `"messages"`, `"stream":true,"messages"`, 1))
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(body, `"content":"Hello from Gemini"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("chat 流式不对: %d %q", resp.StatusCode, body)
	}
	// responses 非流式
	resp, body = postJSON(t, app, "/v1/responses", `{"model":"gem/gemini-neon","input":"hi"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, `"text":"Hello from Gemini"`) {
		t.Fatalf("responses 非流式不对: %d %s", resp.StatusCode, body)
	}
	// /v1/models：直连名带 provider 前缀，上下文声明取内置目录
	resp, body = postJSONWithHeader(t, app, "GET", "/v1/models", "", "Content-Type", "application/json")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &models); err != nil {
		t.Fatal(err)
	}
	var gem map[string]any
	for _, d := range models.Data {
		if d["id"] == "gem/gemini-neon" {
			gem = d
		}
	}
	if gem == nil || gem["context_length"] != 512000.0 {
		t.Errorf("/v1/models 应列出 gem/gemini-neon 并声明上下文: %s", body)
	}
	// 用量按估算值记到该 Provider / 模型（三个请求合并成一行）
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	json.Unmarshal(s.store.SnapshotJSON(), &st)
	if len(st.Usage) != 1 || st.Usage[0].ProviderID != p.ID || st.Usage[0].Model != "gemini-neon" ||
		st.Usage[0].Requests != 3 || st.Usage[0].PromptTokens <= 0 || st.Usage[0].CompletionTokens <= 0 {
		t.Errorf("用量应记到 Gemini Provider: %+v", st.Usage)
	}
}

// 路由组内 Gemini 限流：自动静音该模型条目并故障转移到备用上游
func TestGeminiAppGroupFailoverAndMute(t *testing.T) {
	s, app, fr := newGeminiAppServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"from backup"}}]}`)
	}))
	t.Cleanup(up.Close)
	p, m := addGeminiProvider(t, s, 20)
	backup := addProviderModel(t, s, up.URL, "gemini-neon", func(m *Model) { m.Priority = 10 })

	fr.setMode("429")
	resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, "X-Session-ID", "gem-failover")
	if resp.StatusCode != 200 || !strings.Contains(body, "from backup") {
		t.Fatalf("Gemini 限流后应故障转移到备用上游: %d %s", resp.StatusCode, body)
	}
	saved, _ := s.store.Model(m.ID)
	if saved.MutedUntil.Before(time.Now()) {
		t.Errorf("429 后 Gemini 模型条目应被自动静音: %+v", saved)
	}
	if _, ok := s.store.Model(backup.ID); !ok {
		t.Errorf("备用模型不应受影响")
	}
	s.store.Lock()
	var muted *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindMuted && l.ProviderID == p.ID {
			muted = l
		}
	}
	s.store.Unlock()
	if muted == nil || !strings.Contains(muted.Message, "已自动静音至") {
		t.Errorf("应记录一条静音日志: %+v", muted)
	}
}

func TestGeminiAppDirectErrorPassthrough(t *testing.T) {
	s, app, fr := newGeminiAppServer(t)
	_, m := addGeminiProvider(t, s, 10)
	ask := `{"model":"gem/gemini-neon","messages":[{"role":"user","content":"hi"}]}`

	fr.setMode("429")
	resp, body := postJSON(t, app, "/v1/chat/completions", ask)
	if resp.StatusCode != 429 || !strings.Contains(body, "NATIVE_RATE_LIMITED") {
		t.Fatalf("直连应透传上游 429 与错误体: %d %s", resp.StatusCode, body)
	}
	// 客户端请求本身不被支持：400，且不触达上游（先解掉上一步触发的静音）
	s.store.ClearMute(m.ID)
	resp, body = postJSON(t, app, "/v1/chat/completions", strings.Replace(ask, `"messages"`, `"native_search":true,"messages"`, 1))
	if resp.StatusCode != 400 || !strings.Contains(body, "native_search") {
		t.Fatalf("不支持的请求应 400: %d %s", resp.StatusCode, body)
	}
	if fr.turns() != 1 {
		t.Errorf("归一化失败的请求不应触达上游, ProcessQuery 次数 = %d", fr.turns())
	}
}

func TestGeminiAppProviderValidation(t *testing.T) {
	s, app, _ := newGeminiAppServer(t)
	// 缺凭证：400 并说明缺哪个字段
	resp, body := postJSON(t, app, "/api/providers", `{"name":"g1","kind":"geminiapp","base_url":"http://x"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "geminiapp.subject") {
		t.Fatalf("缺凭证应 400: %d %s", resp.StatusCode, body)
	}
	// 带凭证：保存后基址固定为 Robin 服务的 ProcessQuery 端点（客户端填的基址不生效）
	resp, body = postJSON(t, app, "/api/providers", `{"name":"g2","kind":"geminiapp","base_url":"http://evil.example","geminiapp":{"subject":"s9","refresh_token":"r","email":"e@x.y"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("带凭证应保存成功: %d %s", resp.StatusCode, body)
	}
	var saved *Provider
	for _, p := range s.store.AllProviders() {
		if p.Name == "g2" {
			saved = &p
		}
	}
	// 生产环境里这里是 https://robinfrontend-pa.googleapis.com（测试注入为假上游）
	if saved == nil || saved.BaseURL != s.geminiOrigin+geminiProcessQuery {
		t.Errorf("基址应固定为官方端点: %+v", saved)
	}
	// 不认识的 kind：提示里列出 geminiapp
	resp, body = postJSON(t, app, "/api/providers", `{"name":"g3","kind":"gemini","base_url":"http://x"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "geminiapp") {
		t.Fatalf("未知 kind 应 400: %d %s", resp.StatusCode, body)
	}
}

func TestGeminiAppModelTestAndAdminRoutes(t *testing.T) {
	s, app, fr := newGeminiAppServer(t)
	p, m := addGeminiProvider(t, s, 10)

	// 模型测试：只读用量查询，不发对话
	resp, body := postJSON(t, app, "/api/models/"+m.ID+"/test", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"level":"ok"`) || !strings.Contains(body, "AI Pro") {
		t.Fatalf("模型测试不对: %d %s", resp.StatusCode, body)
	}
	saved, _ := s.store.Provider(p.ID)
	if saved.GeminiApp.Usage == nil || saved.GeminiApp.Usage.Tier != "pro" {
		t.Errorf("模型测试应顺带刷新用量快照: %+v", saved.GeminiApp.Usage)
	}

	// 刷新 / 条款
	resp, body = postJSON(t, app, "/api/providers/"+p.ID+"/gemini-refresh", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"synced":3`) {
		t.Fatalf("刷新不对: %d %s", resp.StatusCode, body)
	}
	resp, body = postJSON(t, app, "/api/providers/"+p.ID+"/gemini-terms", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"status":"accepted"`) {
		t.Fatalf("接受条款不对: %d %s", resp.StatusCode, body)
	}
	// 登录会话：发起 → 轮询 → 取消
	resp, body = postJSON(t, app, "/api/gemini/login", `{"name":"","proxy":""}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"status":"pending"`) || !strings.Contains(body, s.geminiOAuthBase+"/auth?") {
		t.Fatalf("发起登录不对: %d %s", resp.StatusCode, body)
	}
	var login struct {
		State string `json:"state"`
	}
	json.Unmarshal([]byte(body), &login)
	if resp, body = postJSONWithHeader(t, app, "GET", "/api/gemini/login/status?state="+login.State, "", "Content-Type", "application/json"); resp.StatusCode != 200 || !strings.Contains(body, `"pending"`) {
		t.Fatalf("登录状态应为 pending: %d %s", resp.StatusCode, body)
	}
	if resp, _ = postJSON(t, app, "/api/gemini/login/cancel", `{"state":"`+login.State+`"}`); resp.StatusCode != 200 {
		t.Fatalf("取消登录应 200: %d", resp.StatusCode)
	}
	// 导入：凭证不合法 → 400，不创建 provider
	before := len(s.store.AllProviders())
	if resp, _ := postJSON(t, app, "/api/gemini/import", `{"raw":"{\"access_token\":\"x\"}"}`); resp.StatusCode != 400 {
		t.Fatalf("缺 refresh_token 的导入应 400: %d", resp.StatusCode)
	}
	if len(s.store.AllProviders()) != before {
		t.Errorf("失败的导入不应创建 provider")
	}
	// 后台目录接口：内置模型列表
	resp, body = postJSONWithHeader(t, app, "GET", "/api/providers/"+p.ID+"/models", "", "Content-Type", "application/json")
	if resp.StatusCode != 200 || !strings.Contains(body, "gemini-hydrogen") {
		t.Fatalf("模型目录不对: %d %s", resp.StatusCode, body)
	}
	// 以上后台操作都不发对话
	if fr.turns() != 0 {
		t.Errorf("后台操作不应发起 ProcessQuery, got %d 次", fr.turns())
	}
	// 删除 Provider：先让适配器吊销 Google 授权，再删本地配置
	resp, body = postJSONWithHeader(t, app, "DELETE", "/api/providers/"+p.ID, "", "Content-Type", "application/json")
	if resp.StatusCode != 200 {
		t.Fatalf("删除 Provider 失败: %d %s", resp.StatusCode, body)
	}
	if tokens := fr.revokedTokens(); len(tokens) != 1 || tokens[0] != "rt" {
		t.Errorf("删除时应吊销 refresh token, got %v", tokens)
	}
	if _, ok := s.store.Provider(p.ID); ok {
		t.Errorf("删除后 provider 不应还在")
	}
}
