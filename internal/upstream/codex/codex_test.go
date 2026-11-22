package codex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/store"
	"llmrouter/internal/upstream"
)

// errorsAs 便捷包装
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// ── 出站归一化 ────────────────────────────────────────────────────────────

func TestCodexNormalizeResponses(t *testing.T) {
	body := map[string]any{
		"model":                "gpt-6.1-sol",
		"temperature":          0.7,
		"top_p":                0.9,
		"max_output_tokens":    1024,
		"metadata":             map[string]any{"k": "v"},
		"previous_response_id": "resp_1",
		"store":                true,
		"instructions":         "你是助手",
		"input":                "你好",
		"include":              []any{"reasoning.encrypted_content"},
		"tools": []any{
			map[string]any{"type": "function", "name": "f1"},
			map[string]any{"type": "web_search"},
		},
	}
	codexNormalizeResponses(body)
	if body["stream"] != true || body["store"] != false {
		t.Fatalf("恒为流式且不落盘: stream=%v store=%v", body["stream"], body["store"])
	}
	for _, k := range []string{"temperature", "top_p", "max_output_tokens", "metadata", "previous_response_id"} {
		if _, ok := body[k]; ok {
			t.Fatalf("%s 应被剥除", k)
		}
	}
	in, _ := body["input"].([]any)
	if len(in) != 1 {
		t.Fatalf("input 字符串应转为条目数组: %v", body["input"])
	}
	msg := codexMap(in[0])
	if codexStr(msg["role"]) != "user" || codexStr(msg["type"]) != "message" {
		t.Fatalf("条目形态: %v", in[0])
	}
	if _, ok := body["include"]; ok {
		t.Fatal("include 里只剩 reasoning.encrypted_content 时应整组删除")
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || codexStr(codexMap(tools[0])["name"]) != "f1" {
		t.Fatalf("托管工具应被过滤: %v", body["tools"])
	}
}

func TestCodexNormalizeResponsesItems(t *testing.T) {
	items := []any{
		map[string]any{"type": "message", "role": "system", "content": "sys"},               // system → developer
		map[string]any{"type": "reasoning", "summary": nil},                                 // 无摘要 → 丢弃
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "想过"}}}, // 保留
		map[string]any{"type": "compaction"},                                                // 丢弃
		map[string]any{"type": "message", "role": "assistant", "content": "答"},              // 文本部件化
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": "xx"}}},
		map[string]any{"type": "message", "role": "user", "encrypted_content": nil}, // null 字段删除
	}
	out := codexNormalizeItems(items)
	if len(out) != 4 {
		t.Fatalf("应保留 4 条: %v", out)
	}
	if codexStr(codexMap(out[0])["role"]) != "developer" {
		t.Fatalf("system 应折算 developer: %v", out[0])
	}
	assistant := codexMap(out[2])
	parts, _ := assistant["content"].([]any)
	if len(parts) != 1 || codexStr(codexMap(parts[0])["type"]) != "output_text" {
		t.Fatalf("assistant 文本应转 output_text: %v", assistant["content"])
	}
	user := codexMap(out[3])
	uparts, _ := user["content"].([]any)
	if len(uparts) != 1 || codexStr(codexMap(uparts[0])["type"]) != "input_text" {
		t.Fatalf("密文部件应降级 input_text: %v", user["content"])
	}
	if _, ok := user["encrypted_content"]; ok {
		t.Fatal("null 值字段应删除")
	}
}

// ── chat → responses ─────────────────────────────────────────────────────

func TestCodexChatToResponses(t *testing.T) {
	chat := map[string]any{
		"model":            "gpt-6.1-sol",
		"temperature":      0.5,
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys1"},
			map[string]any{"role": "developer", "content": "sys2"},
			map[string]any{"role": "user", "content": "问"},
			map[string]any{"role": "assistant", "content": "答", "tool_calls": []any{
				map[string]any{"id": "call_1", "function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "结果"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "f", "description": "d", "parameters": map[string]any{"type": "object"}}},
		},
	}
	out, err := codexChatToResponses(chat, "gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	if out["instructions"] != "sys1\n\nsys2" {
		t.Fatalf("system/developer 聚合 instructions: %v", out["instructions"])
	}
	items := codexSlice(out["input"])
	if len(items) != 4 { // user / assistant / function_call / function_call_output
		t.Fatalf("条目数: %v", items)
	}
	fn := codexMap(items[2])
	if codexStr(fn["type"]) != "function_call" || codexStr(fn["call_id"]) != "call_1" {
		t.Fatalf("function_call: %v", fn)
	}
	tools := codexSlice(out["tools"])
	if len(tools) != 1 || codexStr(codexMap(tools[0])["name"]) != "f" {
		t.Fatalf("工具扁平化: %v", out["tools"])
	}
	r := codexMap(out["reasoning"])
	if codexStr(r["effort"]) != "high" {
		t.Fatalf("reasoning_effort 映射: %v", out["reasoning"])
	}
	if _, ok := out["temperature"]; ok {
		t.Fatal("temperature 不应搬运")
	}
	// 归一化后满足 SIWC 约束
	codexNormalizeResponses(out)
	if out["store"] != false || out["stream"] != true {
		t.Fatal("归一化后应 store:false stream:true")
	}
}

// ── 事件 → chat 分片与回程 ────────────────────────────────────────────────

const codexTestSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"你好"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"世界"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}}

`

// codexParseJSON 测试便捷解析
func codexParseJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("解析 %q: %v", s, err)
	}
	return v
}

func TestCodexRelayChatStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	rec := httptest.NewRecorder()
	u, wrote, err := s.relay(strings.NewReader(codexTestSSE), rec, "gpt-6.1-sol", true, false, false)
	if err != nil || !wrote { // 流式成功时必然已写响应头与分片
		t.Fatalf("err=%v wrote=%v", err, wrote)
	}
	out := rec.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"你好"`, `"content":"世界"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if rec.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("流式头: %s", rec.Header().Get("Content-Type"))
	}
	if u.Prompt != 5 || u.Cached != 1 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestCodexRelayChatNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	rec := httptest.NewRecorder()
	u, _, err := s.relay(strings.NewReader(codexTestSSE), rec, "gpt-6.1-sol", false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	res := codexParseJSON(t, rec.Body.String())
	if res["object"] != "chat.completion" {
		t.Fatalf("应聚合为 chat.completion: %s", rec.Body.String())
	}
	choices := codexSlice(res["choices"])
	msg := codexMap(codexMap(choices[0])["message"])
	if codexStr(msg["content"]) != "你好世界" {
		t.Fatalf("聚合内容: %s", rec.Body.String())
	}
	if u.Prompt != 5 || u.Completion != 2 {
		t.Fatalf("记账用量: %+v", u)
	}
}

func TestCodexRelayResponsesStreamPassthrough(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	rec := httptest.NewRecorder()
	u, _, err := s.relay(strings.NewReader(codexTestSSE), rec, "gpt-6.1-sol", true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{"event: response.created", `"delta":"你好"`, "event: response.completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "chat.completion.chunk") {
		t.Fatal("responses 客户端不应收到 chat 分片")
	}
	if u.Prompt != 5 || u.Completion != 2 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestCodexRelayResponsesNonStream(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	rec := httptest.NewRecorder()
	u, _, err := s.relay(strings.NewReader(codexTestSSE), rec, "gpt-6.1-sol", false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	res := codexParseJSON(t, rec.Body.String())
	if codexStr(res["id"]) != "resp_1" {
		t.Fatalf("应回最终 response 对象: %s", rec.Body.String())
	}
	if u.Prompt != 5 {
		t.Fatalf("用量: %+v", u)
	}
}

func TestCodexRelayFailed(t *testing.T) {
	s := &Adapter{rt: &upstream.Runtime{}}
	sse := `event: response.failed
data: {"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}}

`
	rec := httptest.NewRecorder()
	_, _, err := s.relay(strings.NewReader(sse), rec, "m", false, true, true)
	if err == nil || !strings.Contains(err.Error(), "usage_limit") {
		t.Fatalf("应报 response.failed 错误: %v", err)
	}
}

// ── JWKS 验证 ────────────────────────────────────────────────────────────

// codexSignJWT 用 RSA 私钥签一个 RS256 JWT（测试铸造 ID token）
func codexSignJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestCodexJWKSVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"k1","n":"%s","e":"AQAB"}]}`,
		base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, jwks)
	}))
	t.Cleanup(srv.Close)
	cli := srv.Client()

	mint := func(nonce string, exp time.Time) string {
		return codexSignJWT(t, key, "k1", map[string]any{
			"iss": codexIssuer, "aud": []string{"oaiapp_x"}, "sub": "user-1",
			"email": "a@b.c", "nonce": nonce, "exp": exp.Unix(),
		})
	}
	ok, err := (&jwksCache{}).verify(context.Background(), cli, srv.URL, mint("n1", time.Now().Add(time.Hour)), "oaiapp_x", "n1")
	if err != nil {
		t.Fatalf("合法 token 应通过: %v", err)
	}
	if ok.Subject != "user-1" || ok.Email != "a@b.c" {
		t.Fatalf("声明: %+v", ok)
	}
	bad := map[string]string{
		"nonce 不匹配": mint("n2", time.Now().Add(time.Hour)),
		"已过期":       mint("n1", time.Now().Add(-time.Hour)),
	}
	for name, tok := range bad {
		if _, err := (&jwksCache{}).verify(context.Background(), cli, srv.URL, tok, "oaiapp_x", "n1"); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
	tampered := mint("n1", time.Now().Add(time.Hour))
	tampered = tampered[:len(tampered)-4] + "AAAA"
	if _, err := (&jwksCache{}).verify(context.Background(), cli, srv.URL, tampered, "oaiapp_x", "n1"); err == nil {
		t.Fatal("签名被篡改应被拒绝")
	}
	// 受众不符
	tok := mint("n1", time.Now().Add(time.Hour))
	if _, err := (&jwksCache{}).verify(context.Background(), cli, srv.URL, tok, "oaiapp_other", "n1"); err == nil {
		t.Fatal("aud 不符应被拒绝")
	}
}

// ── 登录 / 续期 / 数据面 e2e（伪造 auth 与 API 服务） ──────────────────────

// codexTestNonce completeLogin 校验的 nonce 由会话随机生成；测试里把会话 nonce
// 改写成这个常量，伪造 token 端点用它铸造 ID token。
const codexTestNonce = "test-nonce"

// codexFakeAuth 伪造 auth.openai.com：token / jwks / discovery
type codexFakeAuth struct {
	base          string
	key           *rsa.PrivateKey
	wantChallenge string // 非 nil 时校验 PKCE：S256(code_verifier) == 它
	mu            sync.Mutex
	tokens        int // token 端点被调次数
	lastForm      url.Values
}

func codexStartFakeAuth(t *testing.T) *codexFakeAuth {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	fa := &codexFakeAuth{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"k1","n":"%s","e":"AQAB"}]}`,
			base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"revocation_endpoint":%q}`, fa.base+"/api/accounts/oauth/revoke")
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		fa.mu.Lock()
		fa.tokens++
		n := fa.tokens
		fa.lastForm = r.PostForm
		challenge := fa.wantChallenge
		fa.mu.Unlock()
		// PKCE：授权请求的 code_challenge 与本次 code_verifier 应配对
		if r.PostForm.Get("grant_type") == "authorization_code" && challenge != "" {
			d := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(d[:]) != challenge {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_grant","error_description":"pkce mismatch"}`)
				return
			}
		}
		idTok := codexSignJWT(t, key, "k1", map[string]any{
			"iss": codexIssuer, "aud": []string{r.PostForm.Get("client_id")}, "sub": "auth0|user-1",
			"email": "alice@example.com", "nonce": codexTestNonce, "exp": time.Now().Add(time.Hour).Unix(),
		})
		io.WriteString(w, fmt.Sprintf(`{"access_token":"at-%d","refresh_token":"rt-%d","id_token":%q,"token_type":"Bearer","expires_in":3600,"scope":%q}`,
			n, n, idTok, codexScope))
	})
	mux.HandleFunc("/api/accounts/oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fa.base = srv.URL
	return fa
}

// codexFakeAPI 伪造 api.openai.com：models / responses
type codexFakeAPI struct {
	base      string
	responses func(w http.ResponseWriter, r *http.Request) // /responses 覆盖
	lastBody  []byte
	auth      string // 收到的 Authorization
}

func codexStartFakeAPI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *codexFakeAPI {
	t.Helper()
	fi := &codexFakeAPI{responses: handler}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[
			{"slug":"gpt-6.1-sol","display_name":"GPT-6.1","visibility":"list"},
			{"slug":"gpt-6.2-sol","display_name":"GPT-6.2","visibility":"list"},
			{"slug":"codex-mini-pro","display_name":"Codex Mini","visibility":"list"},
			{"slug":"hidden-1","visibility":"hidden"}]}`)
	})
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fi.lastBody = b
		fi.auth = r.Header.Get("Authorization")
		if fi.responses != nil {
			fi.responses(w, r)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fi.base = srv.URL
	return fi
}

// codexNewAdapter 构造注入了伪造端点的适配器与真实 store
func codexNewAdapter(t *testing.T, authBase, apiBase string) (*Adapter, *store.Store) {
	t.Helper()
	st, err := store.LoadStore(t.TempDir() + "/data.json")
	if err != nil {
		t.Fatal(err)
	}
	return NewWithBases(&upstream.Runtime{Store: st, Clients: upstream.NewClientFactory(&http.Client{})}, authBase, apiBase), st
}

// codexStartLogin 发起登录，返回（state, 回调 URL）。回调 URL 从授权地址的
// redirect_uri 参数里解析（回环监听的端口）。
func codexStartLogin(t *testing.T, a *Adapter, name string) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/codex/login", strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
	req.Header.Set("Content-Type", "application/json")
	a.HandleCodexLoginStart(rec, req)
	if rec.Code != 200 {
		t.Fatalf("发起登录: %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if !strings.Contains(res.URL, "client_id=dynamic_agent_client") {
		t.Fatalf("首次登录应走动态注册: %s", res.URL)
	}
	if !strings.Contains(res.URL, "agent_name_hint=llmrouter") {
		t.Fatalf("应带 agent_name_hint: %s", res.URL)
	}
	if !strings.Contains(res.URL, "chatgpt.tokens.use.direct") {
		t.Fatalf("应请求套餐 scope: %s", res.URL)
	}
	if a.rt.Store.CodexHostID == "" || !strings.HasPrefix(a.rt.Store.CodexHostID, "urn:uuid:") {
		t.Fatalf("host ID 应已生成: %q", a.rt.Store.CodexHostID)
	}
	if !strings.Contains(res.URL, "ext_agent_host_id="+url.QueryEscape(a.rt.Store.CodexHostID)) {
		t.Fatalf("授权地址应带 host ID: %s", res.URL)
	}
	// 会话 nonce 改写成伪造 token 端点铸造的值（真实流程里两端各自随机生成）
	a.loginMu.Lock()
	sess := a.logins[res.State]
	a.loginMu.Unlock()
	sess.mu.Lock()
	sess.nonce = codexTestNonce
	sess.mu.Unlock()
	au, err := url.Parse(res.URL)
	if err != nil {
		t.Fatal(err)
	}
	return res.State, au.Query().Get("redirect_uri")
}

// codexWaitStatus 轮询登录会话状态直到非 pending
func codexWaitStatus(t *testing.T, a *Adapter, state string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := htttestGet("/api/codex/login/status?state=" + state)
		a.HandleCodexLoginStatus(rec, req)
		if rec.Code != 200 {
			t.Fatalf("状态: %d %s", rec.Code, rec.Body.String())
		}
		var status map[string]any
		json.Unmarshal(rec.Body.Bytes(), &status)
		if status["status"] != "pending" {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("登录会话一直 pending")
	return nil
}

func htttestGet(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }

func TestCodexLoginFlow(t *testing.T) {
	fa := codexStartFakeAuth(t)
	fi := codexStartFakeAPI(t, nil)
	a, st := codexNewAdapter(t, fa.base, fi.base)

	state, callbackBase := codexStartLogin(t, a, "")
	// 无 code 的请求 404（favicon 等）
	if resp, err := http.Get(strings.TrimRight(callbackBase, "/") + "/favicon.ico"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("favicon 应 404: %d", resp.StatusCode)
		}
	}
	// PKCE：把授权地址里的 code_challenge 交给伪造 token 端点校验
	rec := httptest.NewRecorder()
	_ = rec
	a.loginMu.Lock()
	sess := a.logins[state]
	a.loginMu.Unlock()
	fa.mu.Lock()
	fa.wantChallenge = sess.challenge
	fa.mu.Unlock()
	// 模拟浏览器重定向到回环监听（触发 code 交换 + ID token 校验 + 模型同步）
	go func() {
		resp, err := http.Get(callbackBase + "?code=auth-code-1&state=" + state + "&client_id=oaiapp_issued1")
		if err == nil {
			resp.Body.Close()
		}
	}()

	status := codexWaitStatus(t, a, state)
	if status["status"] != "success" {
		t.Fatalf("登录应成功: %v", status)
	}
	providers := st.AllProviders()
	if len(providers) != 1 {
		t.Fatalf("应创建 1 个 provider: %d", len(providers))
	}
	p := providers[0]
	if p.Kind != "codex" || p.Codex == nil {
		t.Fatalf("provider 形态: %+v", p)
	}
	if p.Codex.Email != "alice@example.com" || p.Codex.Subject != "auth0|user-1" {
		t.Fatalf("账号身份: %+v", p.Codex)
	}
	if p.Codex.ClientID != "oaiapp_issued1" {
		t.Fatalf("应保存回调下发的 client_id: %q", p.Codex.ClientID)
	}
	if p.Codex.AccessToken != "at-1" || p.Codex.RefreshToken != "rt-1" {
		t.Fatalf("token: %+v", p.Codex)
	}
	if !p.Codex.PlanUsage {
		t.Fatal("scopes 含 chatgpt.tokens.use.direct 时 PlanUsage 应为 true")
	}
	if p.BaseURL != fi.base+"/responses" {
		t.Fatalf("基址: %s", p.BaseURL)
	}
	if got := st.ProviderModelCount(p.ID); got != 3 { // hidden 被过滤
		t.Fatalf("模型同步: %d", got)
	}
	// token 端点收到的交换请求形状
	fa.mu.Lock()
	form := fa.lastForm
	fa.mu.Unlock()
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "auth-code-1" ||
		form.Get("client_id") != "oaiapp_issued1" || form.Get("resource") != codexResource ||
		form.Get("code_verifier") == "" || form.Get("redirect_uri") == "" {
		t.Fatalf("code 交换表单: %v", form)
	}

	// 同一账号再登录：原地更新（不新建 provider）
	state2, cb2 := codexStartLogin(t, a, "")
	fa.mu.Lock()
	fa.wantChallenge = ""
	fa.mu.Unlock()
	resp, err := http.Get(cb2 + "?code=auth-code-2&state=" + state2 + "&client_id=oaiapp_issued1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if status := codexWaitStatus(t, a, state2); status["status"] != "success" {
		t.Fatalf("二次登录应成功: %v", status)
	}
	if len(st.AllProviders()) != 1 {
		t.Fatalf("同账号应原地更新: %d", len(st.AllProviders()))
	}
}

func TestCodexLoginPasteFallback(t *testing.T) {
	fa := codexStartFakeAuth(t)
	fi := codexStartFakeAPI(t, nil)
	a, st := codexNewAdapter(t, fa.base, fi.base)
	state, cb := codexStartLogin(t, a, "我的Codex")
	// 无浏览器：把回调 URL 粘贴进来
	u := cb + "?code=paste-code&state=" + state + "&client_id=oaiapp_paste1"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/codex/login/paste", strings.NewReader(fmt.Sprintf(`{"state":%q,"url":%q}`, state, u)))
	req.Header.Set("Content-Type", "application/json")
	a.HandleCodexLoginPaste(rec, req)
	if rec.Code != 200 {
		t.Fatalf("粘贴登录: %d %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res["status"] != "success" {
		t.Fatalf("粘贴登录应成功: %v", res)
	}
	providers := st.AllProviders()
	if len(providers) != 1 || providers[0].Name != "我的Codex" {
		t.Fatalf("自定义名称应生效: %+v", providers)
	}
	if res["warning"] != nil {
		t.Fatalf("模型同步不应有告警: %v", res["warning"])
	}
	if got := st.ProviderModelCount(providers[0].ID); got != 3 {
		t.Fatalf("粘贴登录也应同步模型: %d", got)
	}
}

func TestCodexParseCallbackRejections(t *testing.T) {
	if _, _, err := codexParseCallback("http://127.0.0.1:1"+codexCallback+"?code=c&state=other", "s1"); err == nil {
		t.Fatal("state 不匹配应被拒绝")
	}
	if _, _, err := codexParseCallback("http://127.0.0.1:1"+codexCallback+"?error=access_denied&state=s1", "s1"); err == nil {
		t.Fatal("access_denied 应转错误")
	}
	if _, _, err := codexParseCallback("http://example.com"+codexCallback+"?code=c&state=s1", "s1"); err == nil {
		t.Fatal("非回环应被拒绝")
	}
	if _, _, err := codexParseCallback("http://127.0.0.1:1"+codexCallback+"?state=s1", "s1"); err == nil {
		t.Fatal("缺 code 应被拒绝")
	}
}

func TestCodexRefreshRotation(t *testing.T) {
	fa := codexStartFakeAuth(t)
	a, st := codexNewAdapter(t, fa.base, "")
	exp := time.Now().Add(time.Hour)
	p := &Provider{ID: "p1", Name: "codex1", Kind: "codex", BaseURL: "http://x/responses",
		Codex: &CodexCred{Subject: "s", ClientID: "oaiapp_x", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: &exp}}
	st.UpsertProvider(p)
	// 未临期：直接返回
	nc, err := a.refreshToken(*p, false)
	if err != nil || nc.AccessToken != "old-at" {
		t.Fatalf("未临期不应刷新: %v", err)
	}
	// 临期：续期并轮换
	p.Codex.ExpiresAt = ptrTime(time.Now().Add(time.Minute))
	st.UpsertProvider(p)
	nc, err = a.refreshToken(*p, false)
	if err != nil {
		t.Fatal(err)
	}
	if nc.AccessToken != "at-1" || nc.RefreshToken != "rt-1" {
		t.Fatalf("续期结果: %+v", nc)
	}
	fa.mu.Lock()
	form := fa.lastForm
	fa.mu.Unlock()
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-rt" ||
		form.Get("client_id") != "oaiapp_x" || form.Get("resource") != codexResource {
		t.Fatalf("续期表单: %v", form)
	}
	if form.Get("scope") != "" {
		t.Fatal("续期不应带 scope（保留原授权）")
	}
	// 落盘检查
	saved, _ := st.Provider("p1")
	if saved.Codex.RefreshToken != "rt-1" {
		t.Fatalf("轮换后的 refresh token 应落盘: %q", saved.Codex.RefreshToken)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ── 数据面 e2e：chat / responses × 流式 / 非流式 ─────────────────────────

func codexSSEHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, codexTestSSE)
}

// codexAttemptRun 用固定的 provider/model 跑一次 attempt（凭证不临期，跳过续期）
func codexAttemptRun(t *testing.T, a *Adapter, st *store.Store, body string, responsesMode bool) *httptest.ResponseRecorder {
	t.Helper()
	exp := time.Now().Add(time.Hour)
	st.UpsertProvider(&Provider{ID: "p1", Name: "codex1", Kind: "codex", BaseURL: a.api() + "/responses",
		Codex: &CodexCred{Subject: "s", ClientID: "c", AccessToken: "at", RefreshToken: "rt", ExpiresAt: &exp}})
	st.UpsertModel(&Model{ID: "m1", ProviderID: "p1", Name: "gpt-6.1-sol"})
	p, _ := st.Provider("p1")
	m, _ := st.Model("m1")
	c := Candidate{Provider: p, Model: m}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	sm := &domain.CallSample{}
	if err := a.attempt(rec, req, c, []byte(body), "k", responsesMode, upstream.NewTiming(), sm); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	return rec
}

func TestCodexAttemptChatStream(t *testing.T) {
	fi := codexStartFakeAPI(t, codexSSEHandler)
	a, st := codexNewAdapter(t, "", fi.base)
	body := `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	rec := codexAttemptRun(t, a, st, body, false)
	out := rec.Body.String()
	for _, want := range []string{`"content":"你好"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	// 出站体应已剥掉 temperature 且带 store/stream 约束
	var sent map[string]any
	json.Unmarshal(fi.lastBody, &sent)
	if _, ok := sent["temperature"]; ok {
		t.Fatal("出站不应带 temperature")
	}
	if sent["store"] != false || sent["stream"] != true {
		t.Fatalf("出站约束: %v", sent)
	}
	if fi.auth != "Bearer at" {
		t.Fatalf("鉴权头: %q", fi.auth)
	}
}

func TestCodexAttemptChatNonStream(t *testing.T) {
	fi := codexStartFakeAPI(t, codexSSEHandler)
	a, st := codexNewAdapter(t, "", fi.base)
	body := `{"model":"gpt-6.1-sol","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	rec := codexAttemptRun(t, a, st, body, false)
	res := codexParseJSON(t, rec.Body.String())
	if res["object"] != "chat.completion" {
		t.Fatalf("非流式应聚合: %s", rec.Body.String())
	}
	// system 应折成 instructions 出站
	var sent map[string]any
	json.Unmarshal(fi.lastBody, &sent)
	if sent["instructions"] != "sys" {
		t.Fatalf("instructions: %v", sent["instructions"])
	}
}

func TestCodexAttemptResponsesPassthrough(t *testing.T) {
	fi := codexStartFakeAPI(t, codexSSEHandler)
	a, st := codexNewAdapter(t, "", fi.base)
	body := `{"model":"gpt-6.1-sol","stream":true,"instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	rec := codexAttemptRun(t, a, st, body, true)
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.completed") {
		t.Fatalf("responses 流式应透传事件:\n%s", out)
	}
	var sent map[string]any
	json.Unmarshal(fi.lastBody, &sent)
	items := codexSlice(sent["input"])
	if len(items) != 1 || codexStr(codexMap(items[0])["role"]) != "user" {
		t.Fatalf("input 条目: %v", sent["input"])
	}
}

func TestCodexAttemptUsageLimitMute(t *testing.T) {
	fi := codexStartFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit reached"}}`)
	})
	a, st := codexNewAdapter(t, "", fi.base)
	body := `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`
	exp := time.Now().Add(time.Hour)
	st.UpsertProvider(&Provider{ID: "p1", Name: "codex1", Kind: "codex", BaseURL: fi.base + "/responses",
		Codex: &CodexCred{Subject: "s", ClientID: "c", AccessToken: "at", RefreshToken: "rt", ExpiresAt: &exp}})
	st.UpsertModel(&Model{ID: "m1", ProviderID: "p1", Name: "gpt-6.1-sol"})
	p, _ := st.Provider("p1")
	m, _ := st.Model("m1")
	c := Candidate{Provider: p, Model: m}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	err := a.attempt(rec, req, c, []byte(body), "k", false, upstream.NewTiming(), nil)
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	var se *upstream.StatusError
	if !errorsAs(err, &se) || se.Status != 429 {
		t.Fatalf("应以 StatusError 透传 429: %v", err)
	}
	if !strings.Contains(string(se.Body), "usage_limit_exceeded") {
		t.Fatalf("应保留上游错误体: %s", se.Body)
	}
	saved, _ := st.Model("m1")
	if saved.MutedUntil.IsZero() {
		t.Fatal("用量超限应自动静音")
	}
}

func TestCodexHostIDStable(t *testing.T) {
	_, st := codexNewAdapter(t, "", "")
	id1 := st.EnsureCodexHostID()
	id2 := st.EnsureCodexHostID()
	if id1 == "" || id1 != id2 {
		t.Fatalf("host ID 应稳定: %q vs %q", id1, id2)
	}
	// 重新加载同一文件仍是同一个
	st2, err := store.LoadStore(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st2.EnsureCodexHostID() != id1 {
		t.Fatal("host ID 应跨重启持久")
	}
}
