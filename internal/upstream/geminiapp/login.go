package geminiapp

// Gemini App 账号登录：Google OAuth 回环 PKCE。
//
//  1. 后台发起登录：网关在回环地址监听回调，浏览器打开 Google 授权页
//     （http://localhost:端口/，scope 只征求 flexible-api，要求离线访问）；
//  2. 用户完成 Google 登录与同意后被重定向回回环地址，回调里带授权码；
//  3. 网关用授权码 + PKCE verifier 换 token，随即做一次续期验证 refresh token
//     可用并换到带完整授权范围的 access token，再用 userinfo 取账号身份；
//  4. 按 Google 账号 sub 归并 Provider。
//
// 无浏览器环境（服务器部署）走粘贴兜底：浏览器授权后落地失败的回调 URL 里
// 仍带授权码，把它粘贴进后台即可完成（授权码绑定 PKCE，泄漏给第三方也无法兑换）。

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/httpx"
	"llmrouter/internal/upstream"
)

// geminiLoginMaxSessions 并发登录会话上限（防泄漏：每个会话占一个回环端口）
const geminiLoginMaxSessions = 8

// loginSession 一次进行中/已完成的登录会话（后台轮询用）
type loginSession struct {
	mu           sync.Mutex
	state        string
	challenge    string // PKCE code_challenge
	verifier     string // PKCE code_verifier
	redirectURI  string
	clientID     string // 凭证自带的 OAuth 客户端（续登时沿用；空 = 内置）
	clientSecret string
	name         string // 用户自定义 provider 名（可选）
	proxy        string
	deadline     time.Time
	close        func() // 关闭回环监听
	status       string // pending / success / failed
	err          string // 失败原因
	provider     string // 成功后的 provider ID
}

// loginLookup 取登录会话；过期未完成的当场标记失败并关闭监听
func (a *Adapter) loginLookup(state string) *loginSession {
	a.loginMu.Lock()
	sess := a.logins[state]
	a.loginMu.Unlock()
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.status == "pending" && a.rt.NowTime().After(sess.deadline) {
		sess.status = "failed"
		sess.err = "登录超时（超过 10 分钟未完成 Google 授权）"
		a.loginCloseListener(sess)
	}
	return sess
}

func (a *Adapter) loginCloseListener(sess *loginSession) {
	if sess.close != nil {
		sess.close()
	}
}

// authorizationURL 浏览器要打开的 Google 授权地址
func (a *Adapter) authorizationURL(sess *loginSession) string {
	q := url.Values{}
	q.Set("client_id", a.clientIDFor(&AppCred{ClientID: sess.clientID}))
	q.Set("redirect_uri", sess.redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", oauthLoginScope)
	q.Set("access_type", "offline")
	q.Set("state", sess.state)
	q.Set("code_challenge", sess.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "consent")
	return a.authURL() + "?" + q.Encode()
}

// parseCallback 解析回调 URL：校验回环主机与 state，取出授权码。
// 浏览器回调与粘贴兜底共用；error 参数（如 access_denied）转成错误返回。
// 粘贴内容也可以只有查询串。
func parseCallback(rawURL, wantState string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("回调 URL 为空")
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		// 只粘贴了查询串（可带可不带 ?）
		if !strings.HasPrefix(trimmed, "?") {
			trimmed = "?" + trimmed
		}
		trimmed = "http://localhost/" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("回调 URL 无法解析")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("回调 URL 必须是 http://localhost:端口/?code=… 形态")
	}
	host := u.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if !loopback {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			loopback = true
		}
	}
	if !loopback {
		return "", errors.New("回调 URL 必须指向回环地址（localhost / 127.0.0.1）")
	}
	got := u.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(got), []byte(wantState)) != 1 {
		return "", errors.New("回调 state 不匹配（可能复制了其他会话的回调）")
	}
	if e := strings.TrimSpace(u.Query().Get("error")); e != "" {
		if d := strings.TrimSpace(u.Query().Get("error_description")); d != "" {
			e = e + ": " + d
		}
		if strings.Contains(e, "access_denied") {
			e = "授权被拒绝（未同意 Google 账号授权），可重试"
		}
		return "", errors.New(e)
	}
	code := strings.TrimSpace(u.Query().Get("code"))
	if code == "" {
		return "", errors.New("回调 URL 缺少 code 参数")
	}
	return code, nil
}

// completeLogin 校验回调、换 token、验证续期、取账号身份、落盘 Provider。
// 浏览器回调与粘贴兜底共用；重复调用幂等（首个完成者生效）。
func (a *Adapter) completeLogin(state, callbackURL string) (*loginSession, error) {
	sess := a.loginLookup(state)
	if sess == nil {
		return nil, errors.New("登录会话不存在或已过期")
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.status != "pending" {
		if sess.status == "failed" {
			return sess, errors.New(sess.err)
		}
		return sess, nil
	}
	fail := func(err error) (*loginSession, error) {
		sess.status, sess.err = "failed", err.Error()
		a.loginCloseListener(sess)
		return sess, err
	}
	code, err := parseCallback(callbackURL, sess.state)
	if err != nil {
		return fail(err)
	}
	cli, err := a.rt.Clients.For(Provider{Proxy: sess.proxy}, 30*time.Second)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cred := &AppCred{ClientID: sess.clientID, ClientSecret: sess.clientSecret}
	tr, err := a.exchangeCode(ctx, cli, cred, code, sess.verifier, sess.redirectURI)
	if err != nil {
		if errors.Is(err, errAuthRevoked) {
			return fail(errors.New("授权码无效或已过期（请重新发起登录）"))
		}
		return fail(fmt.Errorf("换取 token 失败: %w", err))
	}
	if tr.RefreshToken == "" {
		return fail(errors.New("Google 没有返回 refresh token，请在授权页完成同意后重试"))
	}
	cred.RefreshToken = tr.RefreshToken
	// 登录即续期：验证 refresh token 可用，并换到带完整授权范围的 access token
	tok, err := a.refreshTokens(ctx, cli, cred)
	if err != nil {
		return fail(fmt.Errorf("验证授权失败: %w", err))
	}
	cred = a.applyToken(cred, tok)
	subject, email, err := a.identify(ctx, cli, cred.AccessToken)
	if err != nil {
		return fail(err)
	}
	cred.Subject, cred.Email = subject, email
	pid := a.persistAccount(cred, sess.name, sess.proxy)
	sess.status, sess.provider = "success", pid
	a.loginCloseListener(sess)
	log.Printf("[INFO] Gemini App 登录成功：%s（provider %s）", credLabel(cred), pid)
	return sess, nil
}

// credLabel 账号的可读标识
func credLabel(c *AppCred) string {
	if c == nil {
		return ""
	}
	if c.Email != "" {
		return c.Email
	}
	return c.Subject
}

// listenLoopback 在回环地址上监听登录回调：优先固定端口（便于无浏览器环境做
// 端口转发），被占用时退回随机端口；同时尽量监听 IPv6 回环（浏览器把
// localhost 解析成 ::1 时也能落地）。返回的两个监听共用同一端口。
func listenLoopback(port int) (ln4, ln6 net.Listener, err error) {
	ln4, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil && port != 0 {
		ln4, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, nil, err
	}
	tcp, ok := ln4.Addr().(*net.TCPAddr)
	if !ok {
		ln4.Close()
		return nil, nil, errors.New("无法取得回环监听端口")
	}
	ln6, _ = net.Listen("tcp", fmt.Sprintf("[::1]:%d", tcp.Port)) // 没有 IPv6 回环时忽略
	return ln4, ln6, nil
}

// ── 管理后台 handler ──────────────────────────────────────────────────────

// HandleGeminiLoginStart 发起登录：生成 PKCE 与回环监听，返回浏览器要打开的
// 授权地址。provider_id 非空且是 Gemini App 类型时沿用其自带的 OAuth 客户端。
func (a *Adapter) HandleGeminiLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Proxy      string `json:"proxy"`
		ProviderID string `json:"provider_id"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	proxy, err := upstream.NormalizeProxy(strings.TrimSpace(req.Proxy))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "网络代理非法: "+err.Error())
		return
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "生成 PKCE 材料失败: "+err.Error())
		return
	}
	sess := &loginSession{
		state: randomHex(16), verifier: verifier, challenge: challenge,
		name: strings.TrimSpace(req.Name), proxy: proxy,
		deadline: a.rt.NowTime().Add(oauthCallbackTimeout), status: "pending",
	}
	if pid := strings.TrimSpace(req.ProviderID); pid != "" {
		if p, ok := a.rt.Store.Provider(pid); ok {
			if cred := Cred(p); cred != nil {
				sess.clientID, sess.clientSecret = cred.ClientID, cred.ClientSecret
			} else {
				httpx.WriteErr(w, http.StatusBadRequest, "该 provider 不是 Gemini App 类型")
				return
			}
		}
	}
	if a.clientIDFor(&AppCred{ClientID: sess.clientID}) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, errOAuthClientMissing.Error())
		return
	}
	ln4, ln6, err := listenLoopback(a.oauthPort)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "回环端口监听失败: "+err.Error())
		return
	}
	port := ln4.Addr().(*net.TCPAddr).Port
	sess.redirectURI = fmt.Sprintf("http://localhost:%d/", port)
	sess.close = func() {
		ln4.Close()
		if ln6 != nil {
			ln6.Close()
		}
	}
	a.loginMu.Lock()
	if a.logins == nil {
		a.logins = map[string]*loginSession{}
	}
	// 先清过期会话；仍超过并发上限则拒绝（每个会话占一个回环端口）
	now := a.rt.NowTime()
	for k, v := range a.logins {
		if v.deadline.Before(now) {
			v.mu.Lock()
			a.loginCloseListener(v)
			v.mu.Unlock()
			delete(a.logins, k)
		}
	}
	if len(a.logins) >= geminiLoginMaxSessions {
		a.loginMu.Unlock()
		sess.close()
		httpx.WriteErr(w, http.StatusConflict, fmt.Sprintf("并发登录会话过多（上限 %d），请稍后再试", geminiLoginMaxSessions))
		return
	}
	a.logins[sess.state] = sess
	a.loginMu.Unlock()
	// 回环监听：浏览器授权后被重定向到这里，捕获回调 URL 并完成登录
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
			// 浏览器落地后会附带请求 favicon 等资源：不带回调参数的请求直接 404
			q := req.URL.Query()
			if strings.TrimSpace(q.Get("code")) == "" && strings.TrimSpace(q.Get("error")) == "" {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			callback := "http://" + req.Host + req.URL.RequestURI()
			rw.Header().Set("Cache-Control", "no-store")
			if _, err := a.completeLogin(sess.state, callback); err != nil {
				rw.Header().Set("Content-Type", "text/html; charset=utf-8")
				rw.WriteHeader(http.StatusBadGateway)
				fmt.Fprintf(rw, "<html><body><p>Google 授权失败：%s</p><p>可回到 llmrouter 后台重试。</p></body></html>", err.Error())
				return
			}
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(rw, "<html><body><p>✅ Google 授权成功，可以关闭本页并回到 llmrouter 后台。</p></body></html>")
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		if ln6 != nil {
			go srv.Serve(ln6) //nolint:errcheck // 监听被会话关闭后返回 ErrServerClosed
		}
		srv.Serve(ln4) //nolint:errcheck // 同上
	}()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"url": a.authorizationURL(sess), "state": sess.state, "status": "pending",
	})
}

// loginStatusPayload 会话状态的统一输出形状
func loginStatusPayload(sess *loginSession) map[string]any {
	out := map[string]any{"state": sess.state, "status": sess.status}
	if sess.err != "" {
		out["error"] = sess.err
	}
	if sess.status == "success" {
		out["provider"] = sess.provider
	}
	return out
}

// HandleGeminiLoginStatus 查询登录会话状态（后台轮询）
func (a *Adapter) HandleGeminiLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess := a.loginLookup(strings.TrimSpace(r.URL.Query().Get("state")))
	if sess == nil {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, loginStatusPayload(sess))
}

// HandleGeminiLoginPaste 无浏览器环境兜底：把浏览器地址栏里的回调 URL 粘贴进来完成登录
func (a *Adapter) HandleGeminiLoginPaste(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "缺少回调 URL")
		return
	}
	sess, err := a.completeLogin(strings.TrimSpace(req.State), strings.TrimSpace(req.URL))
	if err != nil && sess == nil {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, loginStatusPayload(sess))
}

// HandleGeminiLoginCancel 取消登录会话并释放回环端口
func (a *Adapter) HandleGeminiLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	state := strings.TrimSpace(req.State)
	a.loginMu.Lock()
	sess := a.logins[state]
	delete(a.logins, state)
	a.loginMu.Unlock()
	if sess != nil {
		sess.mu.Lock()
		a.loginCloseListener(sess)
		sess.status = "failed"
		sess.mu.Unlock()
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"state": state, "status": "cancelled"})
}

// HandleGeminiImport 粘贴凭证 JSON 直接导入：浏览器登录之外的第二条凭证入口，
// 适配无浏览器环境与跨机器迁移。导入即续期——既验证凭证有效，也确认它仍可续。
func (a *Adapter) HandleGeminiImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Proxy string `json:"proxy"`
		Raw   string `json:"raw"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	proxy, err := upstream.NormalizeProxy(strings.TrimSpace(req.Proxy))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "网络代理非法: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Raw) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "缺少凭证（请粘贴凭证 JSON）")
		return
	}
	cred, err := parseCredential(req.Raw)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.clientIDFor(cred) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, errOAuthClientMissing.Error())
		return
	}
	cli, err := a.rt.Clients.For(Provider{Proxy: proxy}, 30*time.Second)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	tok, err := a.refreshTokens(ctx, cli, cred)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "凭证验证（续期）失败: "+err.Error())
		return
	}
	nc := a.applyToken(cred, tok)
	// 刷新拿到的 access token 换取账号身份，用来归并到 Provider
	subject, email, err := a.identify(ctx, cli, nc.AccessToken)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "读取账号信息失败: "+err.Error())
		return
	}
	nc.Subject, nc.Email = subject, email
	pid := a.persistAccount(nc, strings.TrimSpace(req.Name), proxy)
	log.Printf("[INFO] Gemini App 凭证导入成功：%s（provider %s）", credLabel(nc), pid)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "success", "provider": pid, "expires_at": nc.Expires(),
	})
}

// HandleGeminiRefresh 强制续期并刷新用量快照
func (a *Adapter) HandleGeminiRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := a.rt.Store.Provider(r.PathValue("id"))
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	if Cred(p) == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "该 provider 不是 Gemini App 类型")
		return
	}
	nc, err := a.refreshToken(p, true)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "续期失败: "+err.Error())
		return
	}
	out := map[string]any{
		"expires_at": nc.Expires(),
		"models":     a.rt.Store.ProviderModelCount(p.ID),
	}
	cli, cerr := a.rt.Clients.For(p, 15*time.Second)
	var usage *domain.GeminiAppUsage
	if cerr == nil {
		usage, err = a.fetchUsage(r.Context(), cli, nc.AccessToken)
	} else {
		err = cerr
	}
	a.saveUsage(p.ID, usage, err)
	if err != nil {
		out["warning"] = "用量查询失败: " + err.Error()
	} else {
		out["tier"] = usage.Tier
		out["usage"] = usage
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// HandleGeminiTerms 显式接受 Gemini 主服务条款（用户在后台确认后触发）
func (a *Adapter) HandleGeminiTerms(w http.ResponseWriter, r *http.Request) {
	p, ok := a.rt.Store.Provider(r.PathValue("id"))
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	cred := Cred(p)
	if cred == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "该 provider 不是 Gemini App 类型")
		return
	}
	nc, err := a.refreshToken(p, false)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "续期失败: "+err.Error())
		return
	}
	cli, err := a.rt.Clients.For(p, 15*time.Second)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	version, err := a.acceptTerms(r.Context(), cli, nc.AccessToken)
	terms := &domain.GeminiAppTerms{Status: "accepted", Version: version, AcceptedAt: a.rt.NowTime()}
	if err != nil {
		terms.Status, terms.Error = "error", err.Error()
	}
	a.rt.Store.UpdateGeminiAppTerms(p.ID, terms)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "接受条款失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"terms": terms})
}
