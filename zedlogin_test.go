package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// zedFakeCloud 伪造 cloud.zed.dev：llm_tokens / models / users/me / completions
type zedFakeCloud struct {
	base       string
	jwt        string // 换发的 JWT（zedTestJWT 生成）
	models     string
	account    string
	native     atomic.Value // 收到的原生凭据（llm_tokens 校验后的留档）
	mintMu     sync.Mutex
	mintBodies []string         // 每次 llm_tokens 收到的请求 body（锁定空 body 契约）
	handler    http.HandlerFunc // completions 覆盖（可选）
}

func zedTestJWT(t *testing.T, ttl time.Duration) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(ttl).Unix())))
	return header + "." + payload + ".sig"
}

// zedStartFakeCloud 起一个伪造云端，返回注入了 zedBase 的 Server 与 app
func zedStartFakeCloud(t *testing.T) (*zedFakeCloud, *Server, *httptest.Server) {
	t.Helper()
	fc := &zedFakeCloud{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /client/llm_tokens", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fc.mintMu.Lock()
		fc.mintBodies = append(fc.mintBodies, string(b))
		fc.mintMu.Unlock()
		auth := r.Header.Get("Authorization")
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || parts[0] != "user-1" || !json.Valid([]byte(parts[1])) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"bad native credential"}`)
			return
		}
		fc.native.Store(parts[1])
		fmt.Fprintf(w, `{"token":%q}`, fc.jwt)
	})
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fc.jwt {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(fc.models))
	})
	mux.HandleFunc("GET /client/users/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "user-1 ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(fc.account))
	})
	mux.HandleFunc("POST /completions", func(w http.ResponseWriter, r *http.Request) {
		if fc.handler != nil {
			fc.handler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)
	fc.base = up.URL

	st, err := LoadStore(t.TempDir() + "/data.json")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, client: &http.Client{}, zedBase: up.URL}
	app := httptest.NewServer(s.routes())
	t.Cleanup(app.Close)
	fc.jwt = zedTestJWT(t, time.Hour)
	fc.models = `{"models":[
		{"id":"gpt-5.4","provider":"open_ai"},
		{"id":"claude-sonnet-4.5","provider":"anthropic"},
		{"id":"gemini-3-pro","provider":"google"},
		{"id":"claude-opus-4.6","provider":"anthropic","is_disabled":true},
		{"id":"o4-mini","provider":"open_ai"}]}`
	fc.account = `{"user":{"username":"alice","github_login":"alice-gh"},
		"plan":{"plan_v3":"free","usage":{"model_requests":{"used":3,"limit":{"limited":500}}},
		"subscription_period":{"ended_at":"2026-12-31T00:00:00Z"}}}`
	return fc, s, app
}

// zedInjectLogin 把一个可解密的登录会话塞进 Server（等价于 handleZedLoginStart 的内部装配），
// 返回该会话合法的回调 URL（用会话公钥加密原生凭据）。
func zedInjectLogin(t *testing.T, s *Server, login *zedLogin, state, systemID, name string) string {
	t.Helper()
	sess := &zedLoginSession{
		login: login, systemID: systemID, name: name, state: state,
		deadline: time.Now().Add(zedLoginTimeout), status: "pending",
	}
	sess.close = func() {}
	s.zedLoginMu.Lock()
	if s.zedLogins == nil {
		s.zedLogins = map[string]*zedLoginSession{}
	}
	s.zedLogins[state] = sess
	s.zedLoginMu.Unlock()

	native := []byte(`{"access_token":"native-secret","github_user_login":"alice-gh"}`)
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &login.privateKey.PublicKey, native, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return fmt.Sprintf("http://127.0.0.1:%d/?user_id=user-1&access_token=%s",
		port, base64.RawURLEncoding.EncodeToString(ct))
}

func zedGet(t *testing.T, app *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := app.Client().Get(app.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestZedLoginFlow(t *testing.T) {
	fc, s, app := zedStartFakeCloud(t)
	login, _ := newZedLogin()
	callback := zedInjectLogin(t, s, login, "state-1", "", "我的 Zed")
	if _, err := s.zedCompleteLogin("state-1", callback); err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	p, ok := s.store.ProviderByZedUser("user-1")
	if !ok {
		t.Fatal("登录后应创建 provider")
	}
	if p.Kind != "zed" || p.BaseURL != fc.base+"/completions" {
		t.Fatalf("provider 形态不对: %+v", p)
	}
	if p.Name != "我的 Zed" {
		t.Fatalf("自定义名称未生效: %q", p.Name)
	}
	if p.Zed.Username != "alice" || p.Zed.GitHubLogin != "alice-gh" || p.Zed.Plan == nil || p.Zed.Plan.PlanType != "free" {
		t.Fatalf("账号快照未写入: %+v", p.Zed)
	}
	if p.Zed.AccessToken != fc.jwt {
		t.Fatal("应换得短期 JWT")
	}
	if p.Zed.Plan.Used == nil || *p.Zed.Plan.Used != 3 || p.Zed.Plan.Limit == nil || *p.Zed.Plan.Limit != 500 {
		t.Fatalf("套餐用量解析不对: %+v", p.Zed.Plan)
	}
	// 模型同步：gpt-5.4 / claude-sonnet-4.5 入选；gemini（无线协议）、
	// 禁用的 claude-opus、不带 gpt-/claude- 前缀的 o4-mini 都不建
	want := map[string]bool{"gpt-5.4": true, "claude-sonnet-4.5": true}
	if n := s.store.ProviderModelCount(p.ID); n != len(want) {
		t.Fatalf("模型条目 = %d, want %d", n, len(want))
	}
	for name := range want {
		if !s.store.HasZedModel(p.ID, name) {
			t.Fatalf("缺少模型条目 %s", name)
		}
	}
	for name := range map[string]bool{"gemini-3-pro": true, "claude-opus-4.6": true, "o4-mini": true} {
		if s.store.HasZedModel(p.ID, name) {
			t.Fatalf("不应同步模型 %s", name)
		}
	}

	// 同账号重复登录：原地更新，不新建 provider
	login2, _ := newZedLogin()
	cb2 := zedInjectLogin(t, s, login2, "state-2", "", "")
	if _, err := s.zedCompleteLogin("state-2", cb2); err != nil {
		t.Fatalf("重复登录失败: %v", err)
	}
	p2, ok := s.store.ProviderByZedUser("user-1")
	if !ok || p2.ID != p.ID {
		t.Fatalf("重复登录应原地更新: %q vs %q", p2.ID, p.ID)
	}
	if p2.Name != "我的 Zed" || p2.Zed.Username != "alice" {
		t.Fatal("重复登录应保留名称并刷新账号信息")
	}
	// 会话状态查询
	resp, body := zedGet(t, app, "/api/zed/login/status?state=state-2")
	if resp.StatusCode != 200 || !strings.Contains(body, `"success"`) || !strings.Contains(body, `"models":2`) {
		t.Fatalf("登录状态应成功: %d %s", resp.StatusCode, body)
	}
	// 未完成的会话返回 pending
	login3, _ := newZedLogin()
	zedInjectLogin(t, s, login3, "state-3", "", "")
	_, body = zedGet(t, app, "/api/zed/login/status?state=state-3")
	if !strings.Contains(body, `"pending"`) {
		t.Fatalf("未完成会话应 pending: %s", body)
	}
	// 未知 state → 404
	resp, _ = zedGet(t, app, "/api/zed/login/status?state=nope")
	if resp.StatusCode != 404 {
		t.Fatalf("未知 state 应 404: %d", resp.StatusCode)
	}
}

func TestZedLoginBadCallback(t *testing.T) {
	login, err := newZedLogin()
	if err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"a":1}`)
	ct, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, &login.privateKey.PublicKey, native, nil)
	good := "http://127.0.0.1:99/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(ct)
	if _, _, err := login.decryptCallback(good); err != nil {
		t.Fatalf("合法回调应通过: %v", err)
	}
	for _, bad := range []string{
		"https://127.0.0.1:99/?user_id=u&access_token=x",                                // 非 http
		"http://example.com/?user_id=u&access_token=x",                                  // 非回环
		"http://127.0.0.1:99/callback?user_id=u&access_token=x",                         // 非根路径
		"http://127.0.0.1:99/?user_id=u",                                                // 缺 access_token
		"http://127.0.0.1:99/?access_token=" + base64.RawURLEncoding.EncodeToString(ct), // 缺 user_id
	} {
		if _, _, err := login.decryptCallback(bad); err == nil {
			t.Fatalf("回调 %q 应被拒绝", bad)
		}
	}
	// 他人公钥加密的密文解不开（复制了别的会话的回调）
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	wrong, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, &other.PublicKey, native, nil)
	if _, _, err := login.decryptCallback("http://127.0.0.1:1/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(wrong)); err == nil {
		t.Fatal("他人公钥加密的密文应解密失败")
	}
}

func TestZedMintAndExpiry(t *testing.T) {
	fc, _, _ := zedStartFakeCloud(t)
	cli := &http.Client{}
	cred := &ZedCred{UserID: "user-1", NativeCredential: json.RawMessage(`{"k":1}`)}
	minted, err := zedMintLLMToken(t.Context(), cli, fc.base, cred)
	if err != nil {
		t.Fatalf("重铸失败: %v", err)
	}
	if minted.AccessToken != fc.jwt || minted.ExpiresAt == nil {
		t.Fatalf("重铸结果不对: %+v", minted)
	}
	if !minted.expires().After(time.Now()) {
		t.Fatal("过期时刻应在未来")
	}
	// 契约：llm_tokens body 必须是 {"organization_id":null}
	// （真实上游对空 body / "{}" 都报 400 Invalid JSON）
	fc.mintMu.Lock()
	bodies := append([]string(nil), fc.mintBodies...)
	fc.mintMu.Unlock()
	if len(bodies) != 1 || bodies[0] != `{"organization_id":null}` {
		t.Fatalf("llm_tokens 请求 body 不符: %q", bodies)
	}
	// 无效原生凭据 → 401
	if _, err := zedMintLLMToken(t.Context(), cli, fc.base, &ZedCred{UserID: "bad", NativeCredential: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("无效凭据应失败")
	}
}

func TestZedJWTExpiryParse(t *testing.T) {
	tok := zedTestJWT(t, 90*time.Minute)
	if _, err := zedJWTExpiry(tok); err != nil {
		t.Fatalf("合法 JWT: %v", err)
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d"} {
		if _, err := zedJWTExpiry(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
	// payload 非 JSON 的 JWT
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("!!!!")) + ".x"
	_ = notJSON
}

func TestZedErrorClassification(t *testing.T) {
	if !zedCredentialRejected(401, nil) {
		t.Fatal("401 应判凭据拒绝")
	}
	if !zedCredentialRejected(403, []byte(`{"error":"unauthorized"`)) {
		t.Fatal("不含 plan 的 403 应判凭据拒绝")
	}
	if zedCredentialRejected(403, []byte(`{"error":"your plan does not include this model"}`)) {
		t.Fatal("含 plan 的 403 不应判凭据拒绝")
	}
	if !zedModelPlanRejected(403, []byte(`Upgrade your PLAN`)) {
		t.Fatal("大小写不敏感的 plan 检测")
	}
	if zedModelPlanRejected(403, nil) || zedModelPlanRejected(401, []byte("plan")) {
		t.Fatal("非 403 不算套餐拒绝")
	}
}

func TestZedTokenExpiring(t *testing.T) {
	soon := time.Now().Add(30 * time.Second)
	c := &ZedCred{AccessToken: "t", ExpiresAt: &soon}
	if !c.tokenExpiring() {
		t.Fatal("30 秒后过期应视为临期（窗口 2 分钟）")
	}
	later := time.Now().Add(time.Hour)
	c.ExpiresAt = &later
	if c.tokenExpiring() {
		t.Fatal("1 小时后过期不应临期")
	}
	if !(&ZedCred{}).tokenExpiring() {
		t.Fatal("无 token 应视为临期")
	}
}
