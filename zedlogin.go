package main

// ─────────────────── Zed 原生登录：回环端口 + 临时 RSA 密钥 ───────────────────
//
// 这不是 OAuth code/PKCE。每次登录生成临时 RSA-2048 密钥，把 PKCS#1 DER 公钥
// （base64url）拼进 zed.dev/native_app_signin 的查询串；浏览器完成授权后，
// zed.dev 把「RSA-OAEP/SHA-256 加密的原生凭据」重定向到本机回环端口，网关解密
// 得到长期原生凭据（私钥用完即弃，不落盘）。无浏览器环境（服务器部署）可把浏览器
// 地址栏里的回调 URL 粘贴进后台兜底完成。
//
// 流程参照 ccLoad（MIT）；与 ccLoad 一致，同一账号重复登录原地更新 Provider，
// 并保留已绑定的 system_id 与用户调整过的模型条目。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// zedLoginTimeout 登录会话有效期（浏览器授权的宽限时间）
const zedLoginTimeout = 10 * time.Minute

// zedLoginMaxSessions 并发登录会话上限（防泄漏：每个会话占一个回环端口）
const zedLoginMaxSessions = 8

// zedLogin 一次登录的 RSA 材料
type zedLogin struct {
	privateKey *rsa.PrivateKey
	publicKey  string // PKCS#1 DER → base64url
}

func newZedLogin() (*zedLogin, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("生成登录密钥失败: %v", err)
	}
	return &zedLogin{
		privateKey: key,
		publicKey:  base64.RawURLEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey)),
	}, nil
}

// authorizationURL 浏览器要打开的授权地址（携带回环端口与临时公钥）
func (l *zedLogin) authorizationURL(port int) string {
	q := url.Values{}
	q.Set("native_app_port", strconv.Itoa(port))
	q.Set("native_app_public_key", l.publicKey)
	return zedSignInURL + "?" + q.Encode()
}

// decryptCallback 校验并解密回调 URL，返回 user_id 与原生凭据。
// 只接受回环主机 + 根路径，密文用 RSA-OAEP/SHA-256 解开且必须是合法 JSON。
func (l *zedLogin) decryptCallback(rawURL string) (string, json.RawMessage, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "http") {
		return "", nil, errors.New("回调 URL 必须是 http://127.0.0.1:端口/ 形态")
	}
	host := u.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if !loopback {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			loopback = true
		}
	}
	if !loopback || u.Path != "/" {
		return "", nil, errors.New("回调 URL 必须指向回环地址的根路径")
	}
	userID := strings.TrimSpace(u.Query().Get("user_id"))
	encrypted := strings.TrimSpace(u.Query().Get("access_token"))
	if userID == "" || encrypted == "" {
		return "", nil, errors.New("回调 URL 缺少 user_id 或 access_token 参数")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encrypted, "="))
	if err != nil {
		return "", nil, errors.New("回调 access_token 不是合法 base64url")
	}
	plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, l.privateKey, ciphertext, nil)
	if err != nil {
		return "", nil, errors.New("解密回调 access_token 失败（可能复制了其他会话的回调）")
	}
	if !json.Valid(plaintext) {
		return "", nil, errors.New("解密出的原生凭据不是合法 JSON")
	}
	return userID, json.RawMessage(plaintext), nil
}

// zedLoginSession 一次进行中/已完成的登录会话（后台轮询用）
type zedLoginSession struct {
	mu       sync.Mutex
	login    *zedLogin
	systemID string // 登录时解析出的安装标识
	name     string // 用户自定义 provider 名（可选）
	state    string
	deadline time.Time
	close    func() // 关闭回环监听
	status   string // pending / success / failed
	err      string // 失败原因
	provider string // 成功后的 provider ID
	models   int    // 同步的模型条目数
}

// zedLoginLookup 取登录会话；过期未完成的当场标记失败并关闭监听
func (s *Server) zedLoginLookup(state string) *zedLoginSession {
	s.zedLoginMu.Lock()
	sess := s.zedLogins[state]
	s.zedLoginMu.Unlock()
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

// zedCompleteLogin 解密回调、换取 JWT、拉取模型与账号、落盘 Provider。
// 浏览器回调与粘贴兜底共用；重复调用幂等（首个完成者生效）。
func (s *Server) zedCompleteLogin(state, callbackURL string) (*zedLoginSession, error) {
	sess := s.zedLoginLookup(state)
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
	userID, native, err := sess.login.decryptCallback(callbackURL)
	if err != nil {
		sess.status, sess.err = "failed", err.Error()
		s.zedLoginCloseListener(sess)
		return sess, err
	}
	// 同账号已绑过：沿用其 system_id（安装标识绑定真实安装，不该随本次表单漂移）
	systemID := sess.systemID
	proxy := ""
	if old, ok := s.store.ProviderByZedUser(userID); ok && old.Zed != nil && old.Zed.SystemID != "" {
		systemID = old.Zed.SystemID
		proxy = old.Proxy
	}
	cred := &ZedCred{UserID: userID, SystemID: systemID, NativeCredential: native}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli, err := s.upstreamClient(Provider{Proxy: proxy}, 20*time.Second)
	if err != nil {
		sess.status, sess.err = "failed", err.Error()
		s.zedLoginCloseListener(sess)
		return sess, err
	}
	minted, err := zedMintLLMToken(ctx, cli, s.zedCloud(), cred)
	if err != nil {
		sess.status, sess.err = "failed", "换取推理 JWT 失败: "+err.Error()
		s.zedLoginCloseListener(sess)
		return sess, err
	}
	models, err := zedFetchModels(ctx, cli, s.zedCloud(), minted)
	if err != nil {
		sess.status, sess.err = "failed", "拉取模型目录失败: "+err.Error()
		s.zedLoginCloseListener(sess)
		return sess, err
	}
	account, err := zedFetchAccount(ctx, cli, s.zedCloud(), minted)
	if err != nil {
		sess.status, sess.err = "failed", "拉取账号信息失败: "+err.Error()
		s.zedLoginCloseListener(sess)
		return sess, err
	}
	minted.Username = account.Username
	minted.GitHubLogin = account.GitHubLogin
	minted.Plan = account.zedPlanSnapshot()
	pid, n := s.zedPersistLogin(minted, models, sess.name, proxy)
	sess.status, sess.provider, sess.models = "success", pid, n
	s.zedLoginCloseListener(sess)
	log.Printf("[INFO] Zed 登录成功：%s（provider %s，同步 %d 个模型）", account.Username, pid, n)
	return sess, nil
}

// zedLoginCloseListener 关闭回环监听（nil 安全，锁内调用）
func (s *Server) zedLoginCloseListener(sess *zedLoginSession) {
	if sess.close != nil {
		sess.close()
	}
}

// zedPersistLogin 创建或原地更新 Zed Provider，并同步模型条目（只增不删，
// 不覆盖用户调整过的优先级/上限/暴露开关）。
func (s *Server) zedPersistLogin(cred *ZedCred, models []string, name, proxy string) (string, int) {
	completions := s.zedCloud() + "/completions"
	p := &Provider{Kind: "zed", BaseURL: completions, Proxy: proxy, Zed: cred}
	if old, ok := s.store.ProviderByZedUser(cred.UserID); ok {
		p.ID, p.Name = old.ID, old.Name // 原地更新：保留 ID/名称/代理
	}
	if p.Name == "" {
		p.Name = strings.TrimSpace(name)
	}
	if p.Name == "" {
		label := cred.Username
		if label == "" {
			label = cred.GitHubLogin
		}
		if label == "" {
			label = cred.UserID
			if len(label) > 8 {
				label = label[:8]
			}
		}
		p.Name = "Zed · " + label
	}
	s.store.UpsertProvider(p)
	n := 0
	for _, m := range models {
		if !s.store.HasZedModel(p.ID, m) {
			s.store.UpsertModel(&Model{ProviderID: p.ID, Name: m, Expose: true})
		}
		n++
	}
	return p.ID, n
}

// ── 管理后台 handler ──────────────────────────────────────────────────────

// handleZedLoginStart 发起原生登录：生成 RSA 密钥与回环监听，返回浏览器要打开的
// 授权地址。system_id 可选：表单值 → 环境变量 → 本机 Zed 安装，全部缺失则不带
// 该头，由上游决定账号是否有试用权限。
func (s *Server) handleZedLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SystemID string `json:"system_id"`
		Name     string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	systemID, err := zedResolveSystemID(strings.TrimSpace(req.SystemID))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "system_id 非法: "+err.Error())
		return
	}
	if systemID == "" {
		log.Printf("[WARN] 未取得 Zed system_id（本机无 Zed 安装或读取失败），登录不带安装标识，试用权限由上游决定")
	}
	login, err := newZedLogin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "回环端口监听失败: "+err.Error())
		return
	}
	port := 0
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}
	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)
	sess := &zedLoginSession{
		login: login, systemID: systemID, name: strings.TrimSpace(req.Name),
		state: state, deadline: time.Now().Add(zedLoginTimeout), status: "pending",
	}
	sess.close = func() { ln.Close() }
	s.zedLoginMu.Lock()
	if s.zedLogins == nil {
		s.zedLogins = map[string]*zedLoginSession{}
	}
	// 先清过期会话；仍超过并发上限则拒绝（每个会话占一个回环端口）
	now := time.Now()
	for k, v := range s.zedLogins {
		if v.deadline.Before(now) {
			v.mu.Lock()
			if v.close != nil {
				v.close()
			}
			v.mu.Unlock()
			delete(s.zedLogins, k)
		}
	}
	if len(s.zedLogins) >= zedLoginMaxSessions {
		s.zedLoginMu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Sprintf("并发登录会话过多（上限 %d），请稍后再试", zedLoginMaxSessions))
		return
	}
	s.zedLogins[state] = sess
	s.zedLoginMu.Unlock()
	// 回环监听：浏览器授权后被重定向到这里，捕获回调 URL 并完成登录
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
			callback := "http://" + req.Host + req.URL.RequestURI()
			if _, err := s.zedCompleteLogin(state, callback); err != nil {
				rw.Header().Set("Content-Type", "text/html; charset=utf-8")
				rw.WriteHeader(http.StatusBadGateway)
				fmt.Fprintf(rw, "<html><body><p>Zed 授权失败：%s</p><p>可回到 llmrouter 后台重试。</p></body></html>", err.Error())
				return
			}
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(rw, "<html><body><p>✅ Zed 授权成功，可以关闭本页并回到 llmrouter 后台。</p></body></html>")
		})
		//nolint:gosec // 一次性临时回环监听，无 TLS、生命周期由会话管理
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		srv.Serve(ln) // listener 被会话关闭后返回 ErrClosed，会话随之不再可用
	}()
	writeJSON(w, http.StatusOK, map[string]any{
		"url": login.authorizationURL(port), "state": state, "status": "pending",
	})
}

// handleZedLoginStatus 查询登录会话状态（后台轮询）
func (s *Server) handleZedLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess := s.zedLoginLookup(r.URL.Query().Get("state"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := map[string]any{"state": sess.state, "status": sess.status}
	if sess.err != "" {
		out["error"] = sess.err
	}
	if sess.status == "success" {
		out["provider"] = sess.provider
		out["models"] = sess.models
	}
	writeJSON(w, http.StatusOK, out)
}

// handleZedLoginPaste 无浏览器环境兜底：把浏览器地址栏里的回调 URL 粘贴进来完成登录
func (s *Server) handleZedLoginPaste(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeErr(w, http.StatusBadRequest, "缺少回调 URL")
		return
	}
	sess, err := s.zedCompleteLogin(strings.TrimSpace(req.State), strings.TrimSpace(req.URL))
	if err != nil && sess == nil {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := map[string]any{"state": sess.state, "status": sess.status}
	if sess.err != "" {
		out["error"] = sess.err
	}
	if sess.status == "success" {
		out["provider"] = sess.provider
		out["models"] = sess.models
	}
	writeJSON(w, http.StatusOK, out)
}

// handleZedLoginCancel 取消登录会话并释放回环端口
func (s *Server) handleZedLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if !decode(w, r, &req) {
		return
	}
	state := strings.TrimSpace(req.State)
	s.zedLoginMu.Lock()
	sess := s.zedLogins[state]
	delete(s.zedLogins, state)
	s.zedLoginMu.Unlock()
	if sess != nil {
		sess.mu.Lock()
		s.zedLoginCloseListener(sess)
		sess.status = "failed"
		sess.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "status": "cancelled"})
}

// handleZedRefresh 强制重铸 JWT 并刷新账号套餐快照（镜像 handleWBRefresh）
func (s *Server) handleZedRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Provider(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	cred := zedOf(p)
	if cred == nil {
		writeErr(w, http.StatusBadRequest, "该 provider 不是 Zed 类型")
		return
	}
	nc, err := s.zedRefreshToken(p, true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "重铸失败: "+err.Error())
		return
	}
	// 刷新后的凭证在 store 里已换新，这里补一份账号套餐快照
	cli, cerr := s.upstreamClient(p, 20*time.Second)
	if cerr == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		if account, aerr := zedFetchAccount(ctx, cli, s.zedCloud(), nc); aerr == nil {
			nc.Plan = account.zedPlanSnapshot()
			s.store.UpdateZedAuth(p.ID, nc)
		}
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"expires_at": nc.expires(),
		"plan":       nc.Plan,
		"models":     s.store.ProviderModelCount(p.ID),
	})
}
