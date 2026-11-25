package geminiapp

// 后台账号流程的端到端测试：登录（浏览器回调 / 粘贴回调 / 取消 / state 不符）、
// 凭证导入、刷新与用量快照、条款接受、模型探测。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
)

// modelsOf 某 provider 名下的模型条目（按存储顺序）
func modelsOf(f *fakeGemini, providerID string) []domain.Model {
	f.store.Lock()
	defer f.store.Unlock()
	var out []domain.Model
	for _, m := range f.store.Models {
		if m.ProviderID == providerID {
			out = append(out, *m)
		}
	}
	return out
}

func callHandler(h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("响应不是 JSON（HTTP %d）: %s", rec.Code, rec.Body)
	}
	return v
}

// startLogin 发起登录，返回授权地址与会话 state
func startLogin(t *testing.T, f *fakeGemini, body string) (*url.URL, string) {
	t.Helper()
	rec := callHandler(f.a.HandleGeminiLoginStart, http.MethodPost, "/api/gemini/login", body)
	res := decodeBody(t, rec)
	if rec.Code != 200 || res["status"] != "pending" {
		t.Fatalf("发起登录失败: %d %s", rec.Code, rec.Body)
	}
	raw, _ := res["url"].(string)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u, res["state"].(string)
}

func TestLoginPasteFlow(t *testing.T) {
	f := newFakeGemini(t)
	u, state := startLogin(t, f, `{"name":"我的 Gemini","proxy":""}`)
	// 授权地址：标准 Google OAuth + PKCE；测试里指向假 OAuth 端点
	if wantHost := strings.TrimPrefix(f.oauth.URL, "http://"); u.Host != wantHost || u.Path != "/auth" {
		t.Fatalf("授权地址不对: %s", u)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id": f.a.oauthClientID, "response_type": "code", "scope": oauthLoginScope,
		"access_type": "offline", "code_challenge_method": "S256", "prompt": "consent", "state": state,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://localhost:") || !strings.HasSuffix(redirect, "/") {
		t.Errorf("redirect_uri = %q", redirect)
	}
	if q.Get("code_challenge") == "" {
		t.Errorf("缺少 code_challenge")
	}
	// 回环监听已就绪：不带回调参数的请求 404，不会被当成完成登录
	resp, err := http.Get(strings.Replace(redirect, "localhost", "127.0.0.1", 1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("回环监听对无关请求应 404, got %d", resp.StatusCode)
	}

	// 粘贴回调完成登录：只给查询串也行
	rec := callHandler(f.a.HandleGeminiLoginPaste, http.MethodPost, "/api/gemini/login/paste",
		`{"state":"`+state+`","url":"?state=`+state+`&code=4%2F0AbCdEf&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fflexible-api"}`)
	res := decodeBody(t, rec)
	if rec.Code != 200 || res["status"] != "success" || res["models"] != float64(3) {
		t.Fatalf("粘贴回调应完成登录并同步 3 个模型: %d %s", rec.Code, rec.Body)
	}
	// 换 token：授权码 + PKCE verifier，verifier 与授权地址里的 challenge 对得上
	if len(f.tokenForm) != 2 {
		t.Fatalf("应先后发起换 token 与续期验证两次请求, got %d", len(f.tokenForm))
	}
	exchange := f.tokenForm[0]
	verifier := exchange.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	if exchange.Get("grant_type") != "authorization_code" || exchange.Get("code") != "4/0AbCdEf" ||
		exchange.Get("redirect_uri") != redirect || exchange.Get("client_secret") != f.a.oauthClientSecret ||
		base64.RawURLEncoding.EncodeToString(digest[:]) != q.Get("code_challenge") {
		t.Errorf("换 token 表单不对: %v", exchange)
	}
	if refresh := f.tokenForm[1]; refresh.Get("grant_type") != "refresh_token" || refresh.Get("refresh_token") != "rt-new" {
		t.Errorf("登录后应立即用新 refresh token 续期验证: %v", refresh)
	}
	// Provider 落盘：自定义名称、账号身份、续期后的 token、三个对外模型
	providers := f.store.AllProviders()
	if len(providers) != 1 {
		t.Fatalf("应创建 1 个 provider, got %d", len(providers))
	}
	p := providers[0]
	if p.Kind != Kind || p.Name != "我的 Gemini" || p.BaseURL != f.robin.URL+processQueryPath {
		t.Errorf("provider 基本信息不对: %+v", p)
	}
	c := p.GeminiApp
	if c.Subject != "sub-1" || c.Email != "someone@example.com" || c.RefreshToken != "rt-new" || c.AccessToken != "tok-refreshed" ||
		c.Expires().Before(time.Now()) || c.CheckedAt == nil {
		t.Errorf("凭证不对: %+v", c)
	}
	models := modelsOf(f, p.ID)
	if len(models) != 3 || models[0].Name != "gemini-neon" || !models[0].Expose || models[0].ContextLength != 512000 ||
		models[2].Name != "gemini-flash-lite" || models[2].ContextLength != 0 {
		t.Errorf("模型目录同步不对: %+v", models)
	}
	// 完成后回环监听关闭
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(strings.Replace(redirect, "localhost", "127.0.0.1", 1), "http://"), 300*time.Millisecond); err == nil {
		conn.Close()
		t.Errorf("登录完成后回环监听应关闭")
	}
	// 再查状态：仍是成功态（幂等）
	rec = callHandler(f.a.HandleGeminiLoginStatus, http.MethodGet, "/api/gemini/login/status?state="+state, "")
	if res := decodeBody(t, rec); res["status"] != "success" || res["provider"] != p.ID {
		t.Errorf("状态查询不对: %s", rec.Body)
	}
}

// 浏览器直接打到回环监听：完成登录并返回成功页
func TestLoginBrowserCallback(t *testing.T) {
	f := newFakeGemini(t)
	f.a.oauthPort = 0 // 随机端口，避免与并发测试或占用冲突
	u, state := startLogin(t, f, `{}`)
	code := u.Query().Get("redirect_uri") + "?state=" + state + "&code=4%2F0xyz&prompt=consent"
	if _, err := url.Parse(code); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "授权成功") {
		t.Fatalf("浏览器回调应完成登录: %d %s", resp.StatusCode, page)
	}
	if ps := f.store.AllProviders(); len(ps) != 1 || ps[0].Name != "Gemini · someone@example.com" {
		t.Errorf("默认名称应取账号邮箱: %+v", ps)
	}
	// 同一账号重复登录：原地更新，不新建 provider
	u2, state2 := startLogin(t, f, `{}`)
	if _, err := http.Get(u2.Query().Get("redirect_uri") + "?state=" + state2 + "&code=4%2F0again"); err != nil {
		t.Fatal(err)
	}
	if ps := f.store.AllProviders(); len(ps) != 1 {
		t.Fatalf("同一 Google 账号重复登录应原地更新, got %d 个 provider", len(ps))
	}
}

func TestLoginRejectsMismatchedStateAndDeclined(t *testing.T) {
	f := newFakeGemini(t)
	f.a.oauthPort = 0
	_, state := startLogin(t, f, `{}`)
	rec := callHandler(f.a.HandleGeminiLoginPaste, http.MethodPost, "/api/gemini/login/paste", `{"state":"`+state+`","url":"http://localhost:18765/?state=other&code=x"}`)
	res := decodeBody(t, rec)
	if res["status"] != "failed" || !strings.Contains(res["error"].(string), "state") {
		t.Fatalf("state 不符应失败: %s", rec.Body)
	}
	// 授权被拒：回调里带 error
	_, state = startLogin(t, f, `{}`)
	rec = callHandler(f.a.HandleGeminiLoginPaste, http.MethodPost, "/api/gemini/login/paste",
		`{"state":"`+state+`","url":"http://localhost:18765/?state=`+state+`&error=access_denied"}`)
	if res := decodeBody(t, rec); res["status"] != "failed" {
		t.Fatalf("access_denied 应失败: %s", rec.Body)
	}
	// 非 Google 回调（指向外部主机）不接受
	_, state = startLogin(t, f, `{}`)
	rec = callHandler(f.a.HandleGeminiLoginPaste, http.MethodPost, "/api/gemini/login/paste",
		`{"state":"`+state+`","url":"http://evil.example/?state=`+state+`&code=x"}`)
	if res := decodeBody(t, rec); res["status"] != "failed" {
		t.Fatalf("非回环回调应失败: %s", rec.Body)
	}
	if len(f.store.AllProviders()) != 0 {
		t.Errorf("失败的登录不应创建 provider")
	}
	// 取消
	_, state = startLogin(t, f, `{}`)
	rec = callHandler(f.a.HandleGeminiLoginCancel, http.MethodPost, "/api/gemini/login/cancel", `{"state":"`+state+`"}`)
	if rec.Code != 200 {
		t.Fatalf("取消应 200: %d", rec.Code)
	}
	rec = callHandler(f.a.HandleGeminiLoginStatus, http.MethodGet, "/api/gemini/login/status?state="+state, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("取消后会话应不存在, got %d", rec.Code)
	}
}

func TestImportCredential(t *testing.T) {
	f := newFakeGemini(t)
	// 三种粘贴形态都认；同一账号归并到同一个 Provider
	for i, raw := range []string{
		`{"refresh_token":"rt-a","client_id":"cid","client_secret":"sec"}`,
		`{"refreshToken":"rt-b","googleSubject":"sub-1","label":"someone@example.com"}`,
		`{"kind":"geminiapp","name":"x","geminiapp":{"subject":"sub-1","refresh_token":"rt-c","access_token":"stale","expires_at":"2020-01-01T00:00:00Z"}}`,
	} {
		body, _ := json.Marshal(map[string]string{"name": "", "proxy": "", "raw": raw})
		rec := callHandler(f.a.HandleGeminiImport, http.MethodPost, "/api/gemini/import", string(body))
		res := decodeBody(t, rec)
		if rec.Code != 200 || res["status"] != "success" {
			t.Fatalf("形态 %d 导入失败: %d %s", i, rec.Code, rec.Body)
		}
	}
	ps := f.store.AllProviders()
	if len(ps) != 1 {
		t.Fatalf("同一账号的三次导入应归并成 1 个 provider, got %d", len(ps))
	}
	c := ps[0].GeminiApp
	if c.RefreshToken != "rt-c" || c.AccessToken != "tok-refreshed" || c.ClientID != "" {
		t.Errorf("凭证应取最后一次导入并完成续期验证: refresh=%q access=%q client=%q", c.RefreshToken, c.AccessToken, c.ClientID)
	}
	if form := f.tokenForm[len(f.tokenForm)-1]; form.Get("client_id") != f.a.oauthClientID || form.Get("refresh_token") != "rt-c" {
		t.Errorf("环境变量里的 OAuth 客户端应用于续期: %v", form)
	}
	// 自带 OAuth 客户端的凭证：续期时用凭证里的
	rec := callHandler(f.a.HandleGeminiImport, http.MethodPost, "/api/gemini/import",
		`{"raw":"{\"refresh_token\":\"rt-d\",\"client_id\":\"cid\",\"client_secret\":\"sec\"}"}`)
	if rec.Code != 200 {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body)
	}
	if form := f.tokenForm[len(f.tokenForm)-1]; form.Get("client_id") != "cid" || form.Get("client_secret") != "sec" {
		t.Errorf("凭证自带的 OAuth 客户端应优先: %v", form)
	}
	saved, _ := f.store.Provider(ps[0].ID)
	if saved.GeminiApp.ClientID != "cid" || saved.GeminiApp.ClientSecret != "sec" {
		t.Errorf("自定义 OAuth 客户端应保留在凭证里: %+v", saved.GeminiApp)
	}
	if saved.Name != "Gemini · someone@example.com" {
		t.Errorf("同一账号重复导入应保留已有 Provider 名, got %q", saved.Name)
	}
	// 缺 refresh_token / 不是 JSON：400
	for _, raw := range []string{`{"access_token":"x"}`, `not json`} {
		rec := callHandler(f.a.HandleGeminiImport, http.MethodPost, "/api/gemini/import", `{"raw":`+strconvQuote(raw)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("无效凭证 %q 应 400, got %d", raw, rec.Code)
		}
	}
	// 续期被拒：502，不创建 provider
	rec = callHandler(f.a.HandleGeminiImport, http.MethodPost, "/api/gemini/import", `{"raw":"{\"refresh_token\":\"rt-dead\"}"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("失效凭证应 502, got %d %s", rec.Code, rec.Body)
	}
	if n := len(f.store.AllProviders()); n != 1 {
		t.Errorf("失败的导入不应新建 provider, got %d", n)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 没有环境变量也没有凭证自带 client_id 时，登录/导入应明确提示配置
func TestOAuthClientConfigRequired(t *testing.T) {
	f := newFakeGemini(t)
	f.a.oauthClientID = ""
	rec := callHandler(f.a.HandleGeminiLoginStart, http.MethodPost, "/api/gemini/login", `{"name":"","proxy":""}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "LLMROUTER_GEMINI_CLIENT_ID") {
		t.Fatalf("缺少 OAuth 客户端时发起登录应 400 并提示配置: %d %s", rec.Code, rec.Body)
	}
	rec = callHandler(f.a.HandleGeminiImport, http.MethodPost, "/api/gemini/import", `{"raw":"{\"refresh_token\":\"rt\"}"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "LLMROUTER_GEMINI_CLIENT_ID") {
		t.Fatalf("缺少 OAuth 客户端时导入应 400 并提示配置: %d %s", rec.Code, rec.Body)
	}
}

// 删除 Provider 时应尽力吊销 Google 侧 refresh token
func TestForgetRevokesGoogleGrant(t *testing.T) {
	f := newFakeGemini(t)
	p, _ := f.addProvider(nil)
	f.a.Forget(p.ID)
	f.mu.Lock()
	revoked := append([]url.Values(nil), f.revoked...)
	f.mu.Unlock()
	if len(revoked) != 1 || revoked[0].Get("token") != "rt-1" {
		t.Fatalf("应吊销 refresh token, got %v", revoked)
	}
}

// 用量/条款等字段级回写不能覆盖并发续期写入的新 token
func TestUsageTermsDoNotClobberTokens(t *testing.T) {
	f := newFakeGemini(t)
	p, _ := f.addProvider(nil)
	saved, _ := f.store.Provider(p.ID)
	nc := *saved.GeminiApp
	nc.AccessToken, nc.RefreshToken = "tok-new", "rt-new"
	if !f.store.UpdateGeminiAppToken(p.ID, &nc) {
		t.Fatal("token 回写失败")
	}
	f.a.saveUsage(p.ID, &domain.GeminiAppUsage{Tier: "pro", CheckedAt: time.Now()}, nil)
	if !f.store.UpdateGeminiAppTerms(p.ID, &domain.GeminiAppTerms{Status: "accepted"}) {
		t.Fatal("条款回写失败")
	}
	saved, _ = f.store.Provider(p.ID)
	cred := saved.GeminiApp
	if cred.AccessToken != "tok-new" || cred.RefreshToken != "rt-new" {
		t.Errorf("字段级回写不应覆盖新 token: %+v", cred)
	}
	if cred.Usage == nil || cred.Usage.Tier != "pro" || cred.Terms == nil || cred.Terms.Status != "accepted" {
		t.Errorf("用量/条款快照应写入: %+v", cred)
	}
}

func TestRefreshUsageTermsAndProbe(t *testing.T) {
	f := newFakeGemini(t)
	p, _ := f.addProvider(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/providers/x/gemini-refresh", strings.NewReader("{}"))
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	f.a.HandleGeminiRefresh(rec, req)
	res := decodeBody(t, rec)
	if rec.Code != 200 || res["tier"] != "pro" || res["synced"] != float64(3) {
		t.Fatalf("刷新失败: %d %s", rec.Code, rec.Body)
	}
	saved, _ := f.store.Provider(p.ID)
	usage := saved.GeminiApp.Usage
	if saved.GeminiApp.AccessToken != "tok-refreshed" || usage == nil || usage.Tier != "pro" || len(usage.Rows) != 1 ||
		usage.Rows[0].Label != "Gemini 4 Pro" || usage.Rows[0].Percent != 25 || usage.Rows[0].RefreshAt == nil {
		t.Errorf("刷新后凭证与用量快照不对: cred=%+v usage=%+v", saved.GeminiApp, usage)
	}
	if n := len(modelsOf(f, p.ID)); n != 3 {
		t.Errorf("刷新应同步模型目录, got %d", n)
	}

	// 接受条款：先查版本再提交，回执落盘
	req = httptest.NewRequest(http.MethodPost, "/api/providers/x/gemini-terms", strings.NewReader("{}"))
	req.SetPathValue("id", p.ID)
	rec = httptest.NewRecorder()
	f.a.HandleGeminiTerms(rec, req)
	res = decodeBody(t, rec)
	if rec.Code != 200 {
		t.Fatalf("接受条款失败: %d %s", rec.Code, rec.Body)
	}
	saved, _ = f.store.Provider(p.ID)
	if terms := saved.GeminiApp.Terms; terms == nil || terms.Status != "accepted" || terms.Version != 42 {
		t.Errorf("条款回执不对: %+v", terms)
	}

	// 探测：只读查询用量，不发对话
	before := len(f.gotBodies)
	probe, err := f.a.Probe(t.Context(), saved, "gemini-neon")
	if err != nil || probe.Level != "ok" || !strings.Contains(probe.Message, "AI Pro") {
		t.Errorf("探测结果不对: %+v %v", probe, err)
	}
	if probe, _ = f.a.Probe(t.Context(), saved, "gemini-9-ultra"); probe.Level != "warn" || !strings.Contains(probe.Message, "gemini-9-ultra") {
		t.Errorf("不认识的模型名应 warn: %+v", probe)
	}
	if len(f.gotBodies) != before {
		t.Errorf("探测不应发起对话")
	}
	// 授权失效：没有可用 token 必须续期，续期被拒 → 探测 fail 并给出原因
	stale, _ := f.store.Provider(p.ID)
	stale.GeminiApp.RefreshToken, stale.GeminiApp.AccessToken, stale.GeminiApp.ExpiresAt = "rt-dead", "", nil
	f.store.UpdateGeminiAppAuth(p.ID, stale.GeminiApp)
	if probe, _ = f.a.Probe(t.Context(), stale, "gemini-neon"); probe.Level != "fail" || !strings.Contains(probe.Message, "重新登录") {
		t.Errorf("授权失效应 fail 并提示重登: %+v", probe)
	}
}

func TestUsageDecode(t *testing.T) {
	row := func(pct float32, label string) []byte {
		return pbBytes(2, concat(pbFixed32(2, pct), pbString(3, label)))
	}
	msg := concat(pbVarint(2, 4), // AI Plus
		pbBytes(3, concat(pbVarint(1, 1), row(0.5, "窗口 A"), pbBytes(2, pbString(3, "只有名字")))),
		pbBytes(3, pbBytes(2, concat(pbBytes(1, pbTimestampField(1700000000)), pbFixed32(2, 0.015), pbString(3, "窗口 B")))))
	u, err := decodeUsageInfo(msg)
	if err != nil {
		t.Fatal(err)
	}
	if u.Tier != "plus" || len(u.Rows) != 3 {
		t.Fatalf("用量快照不对: %+v", u)
	}
	if u.Rows[0].Label != "窗口 A" || u.Rows[0].Percent != 50 || u.Rows[0].RefreshAt != nil {
		t.Errorf("第 1 行不对: %+v", u.Rows[0])
	}
	if u.Rows[1].Label != "只有名字" || u.Rows[1].Percent != 0 {
		t.Errorf("第 2 行不对: %+v", u.Rows[1])
	}
	if at := u.Rows[2].RefreshAt; at == nil || at.Unix() != 1700000000 || u.Rows[2].Percent != 1.5 {
		t.Errorf("第 3 行不对: %+v", u.Rows[2])
	}
	if _, err := decodeUsageInfo([]byte{0xff}); err == nil {
		t.Errorf("非法消息应报错")
	}
}

func pbTimestampField(seconds uint64) []byte {
	return concat(pbVarint(1, seconds), pbVarint(2, 500000000))
}

func TestListenLoopbackFallsBackWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ln4, ln6, err := listenLoopback(busy.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln4.Close()
	if ln6 != nil {
		defer ln6.Close()
	}
	if ln4.Addr().(*net.TCPAddr).Port == busy.Addr().(*net.TCPAddr).Port {
		t.Errorf("固定端口被占用时应退回随机端口")
	}
}

func TestAttemptUploadRejectsForeignLocation(t *testing.T) {
	f := newFakeGemini(t)
	// 上传服务被诱导返回别处的「上传地址」：不能把图片字节发过去
	f.mu.Lock()
	f.uploadURL = "https://evil.example/upload/steal"
	f.mu.Unlock()
	p, m := f.addProvider(nil)
	body := `{"model":"gemini-neon","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + tinyImage(t, "image/jpeg") + `"}}]}]}`
	_, _, err := f.attempt(p, m, body, false)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("上传地址不在上传服务同源时应拒绝, got %v", err)
	}
	for _, rec := range f.uploadLog {
		if rec.method == http.MethodPut {
			t.Errorf("不应向外部地址上传图片: %+v", rec)
		}
	}
}
