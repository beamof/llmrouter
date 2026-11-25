package codex

// ─────────────────── Codex 套餐登录：OAuth PKCE + 回环回调 ───────────────────
//
// Sign in with ChatGPT 的动态注册流程（公共客户端，无 secret）：
//  1. 首次登录用 client_id=dynamic_agent_client 发起授权，带 agent_name_hint 与
//     本机持久化的 ext_agent_host_id（store.EnsureCodexHostID）；
//  2. 浏览器完成 ChatGPT 账号认证与「用 ChatGPT 套餐」授权后，重定向到
//     http://127.0.0.1:随机端口/auth/callback，回调里带 code 与新下发的
//     client_id（正式 ID，dynamic_agent_client 只是入口不能保存）；
//  3. 用正式 client_id + PKCE verifier 换 token（access 1h / refresh 30 天滚动），
//     校验 ID token（JWKS RS256 签名 + iss/aud/exp/nonce），保存账号身份；
//  4. 再登录同一账号：沿用保存的 client_id，带 id_token_hint 跳过账号选择。
//
// 无浏览器环境（服务器部署）走粘贴兜底：浏览器授权后落地失败的回调 URL 里
// 仍带 code，把它粘贴进后台即可完成（code 绑定 PKCE，泄漏给第三方也无法兑换）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// codexLoginTimeout 登录会话有效期（浏览器授权的宽限时间）
const codexLoginTimeout = 10 * time.Minute

// codexLoginMaxSessions 并发登录会话上限（防泄漏：每个会话占一个回环端口）
const codexLoginMaxSessions = 8

// loginSession 一次进行中/已完成的登录会话（后台轮询用）
type loginSession struct {
	mu          sync.Mutex
	state       string
	nonce       string
	verifier    string // PKCE code_verifier
	challenge   string
	redirectURI string
	clientID    string // 空 = 首次动态注册；非空 = 续登（保存的正式 client_id）
	idTokenHint string
	loginHint   string
	name        string // 用户自定义 provider 名（可选）
	proxy       string
	deadline    time.Time
	close       func() // 关闭回环监听
	status      string // pending / success / failed
	err         string // 失败原因
	provider    string // 成功后的 provider ID
	warning     string // 非致命提示（如缺少套餐推理权限）
}

// codexRandomHex n 字节随机数的十六进制文本（state / nonce）
func codexRandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// codexNewVerifier PKCE code_verifier：64 字节随机数的 base64url（86 字符）
func codexNewVerifier() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge, nil
}

// codexAuthorizationURL 浏览器要打开的授权地址（SIWC Registration and sign-in 第 2 步）
func (a *Adapter) authorizationURL(sess *loginSession, hostID string) string {
	q := url.Values{}
	if sess.clientID == "" {
		q.Set("client_id", codexDynamicID)
		q.Set("agent_name_hint", codexAgentName) // 仅首次动态注册带
	} else {
		q.Set("client_id", sess.clientID)
		if sess.idTokenHint != "" {
			q.Set("id_token_hint", sess.idTokenHint) // 续登：跳过账号选择
		}
		if sess.loginHint != "" {
			q.Set("login_hint", sess.loginHint)
		}
	}
	q.Set("ext_agent_host_id", hostID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", sess.redirectURI)
	q.Set("scope", codexScope)
	q.Set("resource", codexResource)
	q.Set("state", sess.state)
	q.Set("nonce", sess.nonce)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", sess.challenge)
	return a.authorizeURL() + "?" + q.Encode()
}

// codexParseCallback 解析回调 URL：校验回环主机与 state，取出 code 与 client_id。
// 浏览器回调与粘贴兜底共用；error 参数（如 access_denied）转成错误返回。
func codexParseCallback(rawURL, wantState string) (code, clientID string, err error) {
	u, perr := url.Parse(strings.TrimSpace(rawURL))
	if perr != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "http") {
		return "", "", errors.New("回调 URL 必须是 http://127.0.0.1:端口/auth/callback 形态")
	}
	host := u.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if !loopback {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			loopback = true
		}
	}
	if !loopback {
		return "", "", errors.New("回调 URL 必须指向回环地址")
	}
	if s := strings.TrimSpace(u.Query().Get("state")); s != wantState {
		return "", "", errors.New("回调 state 不匹配（可能复制了其他会话的回调）")
	}
	if e := strings.TrimSpace(u.Query().Get("error")); e != "" {
		if d := strings.TrimSpace(u.Query().Get("error_description")); d != "" {
			e = e + ": " + d
		}
		if strings.Contains(e, "access_denied") {
			e = "授权被拒绝（未同意用 ChatGPT 套餐），可在 ChatGPT 设置里调整后重试"
		}
		return "", "", errors.New(e)
	}
	code = strings.TrimSpace(u.Query().Get("code"))
	if code == "" {
		return "", "", errors.New("回调 URL 缺少 code 参数")
	}
	return code, strings.TrimSpace(u.Query().Get("client_id")), nil
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
	if sess.status == "pending" && time.Now().After(sess.deadline) {
		sess.status = "failed"
		sess.err = "登录超时（超过 10 分钟未完成浏览器授权）"
		if sess.close != nil {
			sess.close()
		}
	}
	return sess
}

// completeLogin 校验回调、换 token、校验 ID token 并落盘 Provider。
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
	code, callbackClientID, err := codexParseCallback(callbackURL, sess.state)
	if err != nil {
		return fail(err)
	}
	// 新注册：client_id 由回调下发；续登：沿用会话保存的 client_id，
	// 回调给出了不同的 ID 时拒绝（不该替换所选账号的注册）
	clientID := callbackClientID
	if sess.clientID != "" {
		if clientID != "" && clientID != sess.clientID {
			return fail(fmt.Errorf("回调的 client_id 与所选账号的注册不符"))
		}
		clientID = sess.clientID
	}
	if clientID == "" {
		return fail(errors.New("新注册回调缺少下发的 client_id（注册未完成）"))
	}
	cli, err := a.rt.Clients.For(Provider{Proxy: sess.proxy}, 30*time.Second)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tr, err := a.exchangeCode(ctx, cli, clientID, code, sess.verifier, sess.redirectURI)
	if err != nil {
		return fail(err)
	}
	// 校验 ID token：JWKS RS256 签名 + iss/aud/exp/nonce（SIWC 第 4 步）
	claims, err := a.jwks.verify(ctx, cli, a.jwksURL(), tr.IDToken, clientID, sess.nonce)
	if err != nil {
		return fail(fmt.Errorf("ID token 校验失败: %v", err))
	}
	if tr.AccessToken == "" {
		return fail(errors.New("token 响应缺少 access_token"))
	}
	cred := &CodexCred{
		Subject:     claims.Subject,
		Email:       claims.Email,
		Name:        claims.Name,
		ClientID:    clientID,
		AccessToken: tr.AccessToken,
		IDToken:     tr.IDToken,
	}
	if tr.RefreshToken != "" {
		cred.RefreshToken = tr.RefreshToken
	}
	exp := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	cred.ExpiresAt = &exp
	if tr.ExpiresIn > 0 {
		rexp := time.Now().Add(30 * 24 * time.Hour)
		cred.RefreshExpiresAt = &rexp
	}
	if tr.Scope != "" {
		cred.Scopes = strings.Fields(tr.Scope)
		for _, s := range cred.Scopes {
			if s == codexPlanScope {
				cred.PlanUsage = true
			}
		}
	}
	now := time.Now()
	cred.CheckedAt = &now
	if !cred.PlanUsage {
		sess.warning = "本次授权未包含 ChatGPT 套餐推理权限（chatgpt.tokens.use.direct），推理会被拒绝；可在 ChatGPT 设置里开启后重新登录"
	}
	pid := a.persistLogin(cred, sess.name, sess.proxy)
	sess.status, sess.provider = "success", pid
	a.loginCloseListener(sess)
	label := claims.Email
	if label == "" {
		label = claims.Subject
	}
	log.Printf("[INFO] Codex 套餐登录成功：%s（provider %s）", label, pid)
	return sess, nil
}

// loginCloseListener 关闭回环监听（nil 安全，锁内调用）
func (a *Adapter) loginCloseListener(sess *loginSession) {
	if sess.close != nil {
		sess.close()
	}
}

// persistLogin 创建或原地更新 Codex Provider。不自动创建模型，模型由用户在
// 后台按需添加（添加对话框会拉取上游目录做建议）。
func (a *Adapter) persistLogin(cred *CodexCred, name, proxy string) string {
	p := &Provider{Kind: "codex", BaseURL: a.dataPlaneURL(cred), Proxy: proxy, Codex: cred}
	if old, ok := a.rt.Store.ProviderByCodexSubject(cred.Subject, cred.ClientID); ok {
		p.ID, p.Name, p.Proxy = old.ID, old.Name, old.Proxy // 原地更新：保留 ID/名称/代理
	}
	if p.Name == "" {
		p.Name = strings.TrimSpace(name)
	}
	if p.Name == "" {
		label := cred.Email
		if label == "" {
			label = cred.Name
		}
		if label == "" {
			label = cred.Subject
			if len(label) > 8 {
				label = label[:8]
			}
		}
		if codexIsCLI(cred) {
			p.Name = "Codex CLI · " + label
		} else {
			p.Name = "Codex · " + label
		}
	}
	a.rt.Store.UpsertProvider(p)
	return p.ID
}

// ── 管理后台 handler ──────────────────────────────────────────────────────

// HandleCodexLoginStart 发起登录：生成 PKCE 与回环监听，返回浏览器要打开的
// 授权地址。provider_id 非空且是 Codex 类型时走续登（沿用其 client_id 与
// id_token_hint，跳过账号选择）；否则首次动态注册。
func (a *Adapter) HandleCodexLoginStart(w http.ResponseWriter, r *http.Request) {
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
	sess := &loginSession{
		state: codexRandomHex(16), nonce: codexRandomHex(16),
		name: strings.TrimSpace(req.Name), proxy: proxy,
		deadline: time.Now().Add(codexLoginTimeout), status: "pending",
	}
	if pid := strings.TrimSpace(req.ProviderID); pid != "" {
		if p, ok := a.rt.Store.Provider(pid); ok {
			if cred := codexOf(p); cred != nil {
				sess.clientID = cred.ClientID
				sess.idTokenHint = cred.IDToken
				sess.loginHint = cred.Email
			} else {
				httpx.WriteErr(w, http.StatusBadRequest, "该 provider 不是 Codex 套餐类型")
				return
			}
		}
	}
	verifier, challenge, err := codexNewVerifier()
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "生成 PKCE 材料失败: "+err.Error())
		return
	}
	sess.verifier, sess.challenge = verifier, challenge
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "回环端口监听失败: "+err.Error())
		return
	}
	port := 0
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}
	sess.redirectURI = fmt.Sprintf("http://127.0.0.1:%d%s", port, codexCallback)
	sess.close = func() { ln.Close() }
	hostID := a.rt.Store.EnsureCodexHostID() // 本机持久化的 ext_agent_host_id
	a.loginMu.Lock()
	if a.logins == nil {
		a.logins = map[string]*loginSession{}
	}
	// 先清过期会话；仍超过并发上限则拒绝（每个会话占一个回环端口）
	now := time.Now()
	for k, v := range a.logins {
		if v.deadline.Before(now) {
			v.mu.Lock()
			if v.close != nil {
				v.close()
			}
			v.mu.Unlock()
			delete(a.logins, k)
		}
	}
	if len(a.logins) >= codexLoginMaxSessions {
		a.loginMu.Unlock()
		httpx.WriteErr(w, http.StatusConflict, fmt.Sprintf("并发登录会话过多（上限 %d），请稍后再试", codexLoginMaxSessions))
		return
	}
	a.logins[sess.state] = sess
	a.loginMu.Unlock()
	// 回环监听：浏览器授权后被重定向到这里，捕获回调 URL 并完成登录
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc(codexCallback, func(rw http.ResponseWriter, req *http.Request) {
			// 浏览器落地后会附带请求 favicon 等资源：不带回调参数的请求直接 404，
			// 不能拿去完成登录（否则会把会话误判失败）
			q := req.URL.Query()
			if strings.TrimSpace(q.Get("code")) == "" && strings.TrimSpace(q.Get("error")) == "" {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			callback := "http://" + req.Host + req.URL.RequestURI()
			if _, err := a.completeLogin(sess.state, callback); err != nil {
				rw.Header().Set("Content-Type", "text/html; charset=utf-8")
				rw.WriteHeader(http.StatusBadGateway)
				fmt.Fprintf(rw, "<html><body><p>ChatGPT 授权失败：%s</p><p>可回到 llmrouter 后台重试。</p></body></html>", err.Error())
				return
			}
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(rw, "<html><body><p>✅ ChatGPT 授权成功，可以关闭本页并回到 llmrouter 后台。</p></body></html>")
		})
		//nolint:gosec // 一次性临时回环监听，无 TLS、生命周期由会话管理
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		srv.Serve(ln) // listener 被会话关闭后返回 ErrClosed，会话随之不再可用
	}()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"url": a.authorizationURL(sess, hostID), "state": sess.state, "status": "pending",
	})
}

// codexLoginStatusPayload 会话状态的统一输出形状
func codexLoginStatusPayload(sess *loginSession) map[string]any {
	out := map[string]any{"state": sess.state, "status": sess.status}
	if sess.err != "" {
		out["error"] = sess.err
	}
	if sess.warning != "" {
		out["warning"] = sess.warning
	}
	if sess.status == "success" {
		out["provider"] = sess.provider
	}
	return out
}

// HandleCodexLoginStatus 查询登录会话状态（后台轮询）
func (a *Adapter) HandleCodexLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess := a.loginLookup(r.URL.Query().Get("state"))
	if sess == nil {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, codexLoginStatusPayload(sess))
}

// HandleCodexLoginPaste 无浏览器环境兜底：把浏览器地址栏里的回调 URL 粘贴进来完成登录
func (a *Adapter) HandleCodexLoginPaste(w http.ResponseWriter, r *http.Request) {
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
	httpx.WriteJSON(w, http.StatusOK, codexLoginStatusPayload(sess))
}

// HandleCodexLoginCancel 取消登录会话并释放回环端口
func (a *Adapter) HandleCodexLoginCancel(w http.ResponseWriter, r *http.Request) {
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

// HandleCodexImport 粘贴凭证 JSON 直接导入：浏览器登录之外的第二条凭证入口，
// 适配无浏览器环境与跨机器迁移（SIWC Self-hosted VMs 的凭证转移场景）。
// 兼容 SIWC 凭证记录与本网关 data.json 的 codex 字段两种形态；导入即续期——
// 既验证凭证有效，也把粘贴来的 refresh token 轮换掉（转移即交割，来源端不再可用）。
func (a *Adapter) HandleCodexImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string     `json:"name"`
		Proxy string     `json:"proxy"`
		Raw   string     `json:"raw"`
		Cred  *CodexCred `json:"cred"` // 结构化形态（内部调用/测试）
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	proxy, err := upstream.NormalizeProxy(strings.TrimSpace(req.Proxy))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "网络代理非法: "+err.Error())
		return
	}
	cred := req.Cred
	if cred == nil && strings.TrimSpace(req.Raw) != "" {
		if cred, err = codexParseCred([]byte(strings.TrimSpace(req.Raw)), time.Now()); err != nil {
			httpx.WriteErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if cred == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "缺少凭证（粘贴凭证 JSON 或提供 cred 字段）")
		return
	}
	if strings.TrimSpace(cred.ClientID) == "" || strings.TrimSpace(cred.RefreshToken) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "凭证缺少 client_id / refresh_token，无法导入")
		return
	}
	cli, err := a.rt.Clients.For(Provider{Proxy: proxy}, 30*time.Second)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	// 导入即续期：验证凭证有效，并轮换掉粘贴来的 refresh token
	tr, err := a.refreshTokens(ctx, cli, cred)
	if err != nil {
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream,
			Message: "Codex 凭证导入失败（续期被拒）", Detail: err.Error()})
		httpx.WriteErr(w, http.StatusBadGateway, "凭证验证（续期）失败: "+err.Error())
		return
	}
	// 刷新响应里的 ID token 经 JWKS 校验后回填账号身份（导入不携带 nonce，跳过该项）
	warn := ""
	if tr.IDToken != "" {
		if claims, verr := a.jwks.verify(ctx, cli, a.jwksURL(), tr.IDToken, cred.ClientID, ""); verr == nil {
			cred.Subject, cred.Email, cred.Name = claims.Subject, claims.Email, claims.Name
		} else {
			warn = "ID token 校验失败，沿用凭证自带的账号信息: " + verr.Error()
		}
	}
	if strings.TrimSpace(cred.Subject) == "" {
		// 兜底：解码凭证保留的 id_token 取 sub（粘贴内容本身即受信输入）
		if sub, email, name := codexDecodeIDTokenClaims(cred.IDToken); sub != "" {
			cred.Subject, cred.Email, cred.Name = sub, email, name
		}
	}
	if strings.TrimSpace(cred.Subject) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "凭证缺少 subject（账号标识），无法归并到 Provider")
		return
	}
	nc := a.credFromToken(cred, tr)
	if !nc.PlanUsage && !codexIsCLI(nc) {
		if warn != "" {
			warn += "；"
		}
		warn += "凭证未包含 ChatGPT 套餐推理权限（chatgpt.tokens.use.direct），推理会被拒绝"
	}
	pid := a.persistLogin(nc, strings.TrimSpace(req.Name), proxy)
	label := nc.Email
	if label == "" {
		label = nc.Subject
	}
	log.Printf("[INFO] Codex 凭证导入成功：%s（provider %s）", label, pid)
	out := map[string]any{
		"status": "success", "provider": pid,
		"expires_at": nc.Expires(), "plan_usage": nc.PlanUsage,
	}
	if warn != "" {
		out["warning"] = warn
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// HandleCodexRefresh 强制续期并刷新凭证信息（镜像 HandleZedRefresh）
func (a *Adapter) HandleCodexRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := a.rt.Store.Provider(r.PathValue("id"))
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	if codexOf(p) == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "该 provider 不是 Codex 套餐类型")
		return
	}
	nc, err := a.refreshToken(p, true)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "续期失败: "+err.Error())
		return
	}
	out := map[string]any{
		"expires_at":         nc.Expires(),
		"refresh_expires_at": nc.RefreshExpires(),
		"plan_usage":         nc.PlanUsage,
		"models":             a.rt.Store.ProviderModelCount(p.ID),
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
