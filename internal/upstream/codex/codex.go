package codex

// ─────────────────── Codex 套餐：SIWC 常量、OAuth 与账号 API ───────────────────
//
// "Codex 套餐"指 Sign in with ChatGPT 的 ChatGPT plan usage：ChatGPT Plus / Pro
// 用户把套餐额度授权给开源工具，凭证是 OAuth 公共客户端（无 secret）动态注册
// 下发的 client_id + access/refresh token。推理必须走公共 Responses API
// （https://api.openai.com/v1/responses）并恒带 store:false + stream:true，
// 不支持的字段与工具要在出站前剥掉（见 codexconv.go）。流程与约束对照
// developers.openai.com/siwc 的 Registration and sign-in / Models and inference。

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/upstream"
)

const (
	codexAuthBase  = "https://auth.openai.com"
	codexIssuer    = "https://auth.openai.com"
	codexAPIBase   = "https://api.openai.com/v1"
	codexResource  = "https://api.openai.com/v1"
	codexDynamicID = "dynamic_agent_client" // 首次动态注册入口，不落盘
	codexAgentName = "llmrouter"            // agent_name_hint，全安装一致
	// codexScope 身份 + ChatGPT 套餐推理的完整请求集（授权页按此征求同意）
	codexScope     = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	codexPlanScope = "chatgpt.tokens.use.direct"
	codexCallback  = "/auth/callback" // 回调路径固定，仅端口可变（SIWC 要求）

	codexResponsesPath = "/responses"
	codexModelsPath    = "/models"

	// codexRefreshWindow access token（1 小时）临期窗口：不足 2 分钟时提前续
	codexRefreshWindow = 2 * time.Minute
)

// ── Codex CLI（~/.codex/auth.json）模式的固定参数 ─────────────────────────
//
// Codex CLI 官方客户端的 OAuth app 与数据面端点（与 SIWC plan usage 是两套体系：
// 刷新端点不同、凭证字段嵌套在 tokens 里、推理走 ChatGPT 后端并要求伪装 CLI
// 的专用请求头）。参数与端点为公开事实（Codex CLI / ccLoad 等开源实现一致）。
const (
	codexCLIClientID   = "app_EMoamEEZ73f0CkXaXp7hrann" // Codex CLI 官方公开 client（与 auth.json 内 JWT 的 client_id 一致）
	codexCLIScope      = "openid profile email offline_access"
	codexCLIBase       = "https://chatgpt.com/backend-api/codex"
	codexCLIOriginator = "codex_cli_rs"
	codexCLIUserAgent  = "codex_cli_rs/0.68.0 (Windows 11; x86_64) llmrouter"
)

// codexCLIModels CLI 模式的内置模型目录：ChatGPT 后端没有公开的目录端点，
// 按当前 Codex 命名空间内置（可用集因账号档位而异，free 档可能全部不可用；
// 后台可手填任意模型名，返回 model_not_found 即该账号无此模型授权）。
var codexCLIModels = []string{
	"gpt-5.6-sol",
	"gpt-5.5", "gpt-5.5-codex",
	"gpt-5.4", "gpt-5.4-codex",
	"gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.3-codex-mini",
	"gpt-5.2-codex", "gpt-5.2-codex-mini",
	"gpt-5.1-codex", "gpt-5.1-codex-mini",
}

// tokenResponse OAuth token 端点的响应形状（code 交换与 refresh 共用）。
// Error 用 RawMessage：/oauth/token 的错误是嵌套对象（{"error":{"code",…}}），
// 标准形态是字符串，两种都见过。
type tokenResponse struct {
	AccessToken      string          `json:"access_token"`
	RefreshToken     string          `json:"refresh_token"`
	IDToken          string          `json:"id_token"`
	TokenType        string          `json:"token_type"`
	ExpiresIn        int64           `json:"expires_in"`
	Scope            string          `json:"scope"`
	EarliestRefresh  int64           `json:"earliest_refresh_at"`
	Error            json.RawMessage `json:"error,omitempty"`
	ErrorDescription string          `json:"error_description,omitempty"`
}

// codexOf 返回 provider 的 Codex 凭证；非 codex provider 返回 nil
func codexOf(p Provider) *CodexCred {
	if p.Kind == "codex" {
		return p.Codex
	}
	return nil
}

// codexIsCLI 凭证是否来自 Codex CLI 的 auth.json（数据面与刷新端点不同）
func codexIsCLI(c *CodexCred) bool { return c != nil && c.Mode == "cli" }

// codexTokenURL token 端点（SIWC，authBase 可注入，测试指向 httptest）
func (a *Adapter) tokenURL() string {
	base := a.authBase
	if base == "" {
		base = codexAuthBase
	}
	return strings.TrimRight(base, "/") + "/api/accounts/oauth/token"
}

// codexCLITokenURL Codex CLI 的刷新端点（同一授权服务器、不同路径）
func (a *Adapter) cliTokenURL() string {
	base := a.authBase
	if base == "" {
		base = codexAuthBase
	}
	return strings.TrimRight(base, "/") + "/oauth/token"
}

// codexCLIDataBase CLI 模式的数据面基址（cliBase 可注入）
func (a *Adapter) cliDataBase() string {
	if a.cliBase != "" {
		return strings.TrimRight(a.cliBase, "/")
	}
	return codexCLIBase
}

// codexDataPlaneURL 该凭证的数据面端点（SIWC → 公共 Responses API；
// CLI → ChatGPT 后端的 codex responses）
func (a *Adapter) dataPlaneURL(cred *CodexCred) string {
	if codexIsCLI(cred) {
		return a.cliDataBase() + codexResponsesPath
	}
	return a.api() + codexResponsesPath
}

// codexAuthorizeURL 授权端点
func (a *Adapter) authorizeURL() string {
	base := a.authBase
	if base == "" {
		base = codexAuthBase
	}
	return strings.TrimRight(base, "/") + "/api/accounts/authorize"
}

// codexJWKSURL ID token 签名公钥
func (a *Adapter) jwksURL() string {
	base := a.authBase
	if base == "" {
		base = codexAuthBase
	}
	return strings.TrimRight(base, "/") + "/.well-known/jwks.json"
}

// codexAPIBase 推理与模型目录基址（apiBase 可注入）
func (a *Adapter) api() string {
	if a.apiBase != "" {
		return strings.TrimRight(a.apiBase, "/")
	}
	return codexAPIBase
}

// ── OAuth 端点调用（exchange / refresh / revoke） ─────────────────────────

// codexPostForm 表单 POST + JSON 响应的公共骨架
func codexPostForm(ctx context.Context, cli *http.Client, endpoint string, form url.Values) (*tokenResponse, int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, b, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, resp.StatusCode, b, fmt.Errorf("token 端点返回的不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	return &tr, resp.StatusCode, b, nil
}

// codexExchangeCode 用授权码换 token（公共客户端，无 secret）。
// clientID 用回调下发的正式 client_id（首次）或保存的 client_id（续登）。
func (a *Adapter) exchangeCode(ctx context.Context, cli *http.Client, clientID, code, verifier, redirectURI string) (*tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"resource":      {codexResource},
	}
	tr, status, _, err := codexPostForm(ctx, cli, a.tokenURL(), form)
	if err != nil {
		return nil, fmt.Errorf("换取 token 失败: %v", err)
	}
	if status < 200 || status >= 300 || tr.AccessToken == "" {
		return nil, codexTokenError("换取 token", status, tr)
	}
	return tr, nil
}

// ── 凭证粘贴导入 ─────────────────────────────────────────────────────────

// codexJWTExpiry 解码 JWT 的 exp 声明（Codex CLI 的 auth.json 不带过期时刻，
// 但它的 access token 本身是 JWT，可直接解出）。
func codexJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0), true
}

// codexParseCLIAuth 解析 Codex CLI 的 ~/.codex/auth.json（auth_mode=chatgpt 时
// 的形态：凭证嵌套在 tokens 对象里）。非该形态返回 nil。
func codexParseCLIAuth(raw []byte) *CodexCred {
	var v struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken  string `json:"access_token"`
			AccountID    string `json:"account_id"`
			IDToken      string `json:"id_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Tokens == nil || v.Tokens.RefreshToken == "" {
		return nil
	}
	if v.AuthMode != "" && v.AuthMode != "chatgpt" {
		return nil // API Key 模式的 auth.json 不属于本适配器
	}
	cred := &CodexCred{
		Mode:         "cli",
		AccountID:    strings.TrimSpace(v.Tokens.AccountID),
		ClientID:     codexCLIClientID,
		AccessToken:  strings.TrimSpace(v.Tokens.AccessToken),
		IDToken:      strings.TrimSpace(v.Tokens.IDToken),
		RefreshToken: strings.TrimSpace(v.Tokens.RefreshToken),
		Scopes:       strings.Fields(codexCLIScope),
		PlanUsage:    true, // CLI 登录态本身即 Codex 推理授权
	}
	if sub, email, name := codexDecodeIDTokenClaims(cred.IDToken); sub != "" {
		cred.Subject, cred.Email, cred.Name = sub, email, name
	}
	if exp, ok := codexJWTExpiry(cred.AccessToken); ok {
		cred.ExpiresAt = &exp
	}
	rexp := time.Now().Add(30 * 24 * time.Hour)
	cred.RefreshExpiresAt = &rexp
	return cred
}

// codexParseCred 解析粘贴导入的凭证 JSON。兼容三种形态：
//  1. Codex CLI 的 ~/.codex/auth.json（tokens 嵌套）→ CLI 模式凭证；
//  2. SIWC 官方文档的本地凭证记录（Registration and sign-in 第 5 步）：
//     expires_in 为相对秒数，附带 issuer / ext_agent_host_id / saved_at 等额外字段；
//  3. 本网关 data.json 里 provider 的 codex 字段：expires_at 为绝对时刻。
//
// client_id 与 refresh_token 缺一不可（无法续期的凭证没有意义）；subject 允许
// 缺省，由导入流程从刷新响应的 ID token 或凭证保留的 id_token 补齐。
// 凭证里的 ext_agent_host_id 属于来源主机，导入时不采用（本机沿用自身的 host ID）。
func codexParseCred(raw []byte, now time.Time) (*CodexCred, error) {
	if cli := codexParseCLIAuth(raw); cli != nil {
		return cli, nil
	}
	var f struct {
		Subject          string     `json:"subject"`
		Email            string     `json:"email"`
		Name             string     `json:"name"`
		ClientID         string     `json:"client_id"`
		IDToken          string     `json:"id_token"`
		AccessToken      string     `json:"access_token"`
		RefreshToken     string     `json:"refresh_token"`
		ExpiresIn        int64      `json:"expires_in"`
		ExpiresAt        *time.Time `json:"expires_at"`
		RefreshExpiresAt *time.Time `json:"refresh_expires_at"`
		Scopes           []string   `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, errors.New("凭证 JSON 无法解析（需要对象形态）")
	}
	cred := &CodexCred{
		Subject:          strings.TrimSpace(f.Subject),
		Email:            strings.TrimSpace(f.Email),
		Name:             strings.TrimSpace(f.Name),
		ClientID:         strings.TrimSpace(f.ClientID),
		IDToken:          strings.TrimSpace(f.IDToken),
		AccessToken:      strings.TrimSpace(f.AccessToken),
		RefreshToken:     strings.TrimSpace(f.RefreshToken),
		RefreshExpiresAt: f.RefreshExpiresAt,
	}
	for _, s := range f.Scopes {
		if s = strings.TrimSpace(s); s != "" {
			cred.Scopes = append(cred.Scopes, s)
			if s == codexPlanScope {
				cred.PlanUsage = true
			}
		}
	}
	if cred.ClientID == "" {
		return nil, errors.New("凭证缺少 client_id 字段")
	}
	if cred.RefreshToken == "" {
		return nil, errors.New("凭证缺少 refresh_token 字段")
	}
	switch {
	case f.ExpiresAt != nil:
		cred.ExpiresAt = f.ExpiresAt // 本网关形态：绝对时刻原样保留
	case f.ExpiresIn > 0:
		t := now.Add(time.Duration(f.ExpiresIn) * time.Second) // SIWC 记录形态：按导入时刻折算
		cred.ExpiresAt = &t
	}
	if cred.RefreshExpiresAt == nil {
		t := now.Add(30 * 24 * time.Hour) // refresh token 30 天滚动
		cred.RefreshExpiresAt = &t
	}
	return cred, nil
}

// codexDecodeIDTokenClaims 不验签名地解码 id_token 的身份声明（导入兜底用：
// 粘贴内容本身即受信输入，签名在原始登录时已验证过）。
func codexDecodeIDTokenClaims(token string) (subject, email, name string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}
	var claims codexIDClaims
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", ""
	}
	return strings.TrimSpace(claims.Subject), strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Name)
}

// codexRefreshToken 用 refresh token 续期（轮换：响应带新的 refresh token）。
// SIWC 与 CLI 凭证的刷新端点与表单不同（CLI 走 /oauth/token 并显式带 scope）。
// 失败按错误码分类：invalid_grant / *expired / *invalidated / *reused 表示
// refresh token 已不可用，需要重新登录。
func (a *Adapter) refreshTokens(ctx context.Context, cli *http.Client, cred *CodexCred) (*tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cred.ClientID},
		"refresh_token": {cred.RefreshToken},
	}
	endpoint := a.tokenURL()
	if codexIsCLI(cred) {
		form.Set("scope", codexCLIScope)
		endpoint = a.cliTokenURL()
	} else {
		form.Set("resource", codexResource)
	}
	tr, status, _, err := codexPostForm(ctx, cli, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("续期请求失败: %v", err)
	}
	if status < 200 || status >= 300 || tr.AccessToken == "" {
		return nil, codexTokenError("续期", status, tr)
	}
	return tr, nil
}

// codexTokenError 把 token 端点的 OAuth 错误翻译成可读信息
func codexTokenError(op string, status int, tr *tokenResponse) error {
	code, detail := codexOAuthError(tr)
	if code == "" && detail == "" {
		return fmt.Errorf("%s 返回 HTTP %d", op, status)
	}
	if detail == "" {
		detail = code
	}
	if code == "" {
		code = detail
	}
	if codexRefreshDead(code) {
		return fmt.Errorf("%s失败：%s（授权已失效，请重新登录 ChatGPT 账号）", op, detail)
	}
	return fmt.Errorf("%s失败：%s[%s]（HTTP %d）", op, detail, code, status)
}

// codexOAuthError 兼容两种错误形态：标准 OAuth（error 字符串 + error_description）
// 与 /oauth/token 的嵌套对象（{"error":{"code","message"}}）。
func codexOAuthError(tr *tokenResponse) (code, detail string) {
	raw := bytes.TrimSpace(tr.Error)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, tr.ErrorDescription
	}
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Code, obj.Message
	}
	return "", ""
}

// codexRefreshDead 哪些错误码表示 refresh token 不可再用（SIWC Errors and recovery）
func codexRefreshDead(code string) bool {
	switch code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}

// codexRevoke 尽力吊销可续期会话（删除 Provider 时调用）。吊销端点来自 OIDC
// 发现文档；失败只记日志，不阻断删除（用户也可在 ChatGPT 设置里手动断开）。
func (a *Adapter) revoke(ctx context.Context, cli *http.Client, clientID, refreshToken string) error {
	base := a.authBase
	if base == "" {
		base = codexAuthBase
	}
	discURL := strings.TrimRight(base, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discURL, nil)
	if err != nil {
		return err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var disc struct {
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := json.Unmarshal(b, &disc); err != nil || disc.RevocationEndpoint == "" {
		return errors.New("发现文档里没有 revocation_endpoint")
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	r2, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := cli.Do(r2)
	if err != nil {
		return err
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return fmt.Errorf("吊销返回 HTTP %d", resp2.StatusCode)
	}
	return nil
}

// ── ID token 校验（JWKS 签名 + iss/aud/exp/nonce） ────────────────────────

// codexIDClaims ID token 里网关用到的声明
type codexIDClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Nonce   string `json:"nonce"`
}

// codexJWK 一把 RS256 公钥（RFC 7517 JWK 的 RSA 子集）
type codexJWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwksCache 按 kid 缓存签名公钥；未知 kid 或缓存过旧时重拉
type jwksCache struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

const jwksTTL = 24 * time.Hour

func (c *jwksCache) fetch(ctx context.Context, cli *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS 返回 HTTP %d", resp.StatusCode)
	}
	var v struct {
		Keys []codexJWK `json:"keys"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return errors.New("JWKS 不是合法 JSON")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range v.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) < 1 || len(e) > 8 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		nInt := new(big.Int).SetBytes(n)
		if nInt.BitLen() == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: nInt, E: exp}
	}
	if len(keys) == 0 {
		return errors.New("JWKS 里没有可用的 RSA 公钥")
	}
	c.keys, c.fetched = keys, time.Now()
	return nil
}

// verify 用 JWKS 校验 ID token：RS256 签名、iss、aud、exp、nonce。
// 返回解析出的账号声明；任何一步失败都拒绝（SIWC Registration and sign-in 第 4 步）。
func (c *jwksCache) verify(ctx context.Context, cli *http.Client, jwksURL, token, audience, nonce string) (*codexIDClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("ID token 不是 JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &header) != nil {
		return nil, errors.New("ID token 头部无法解析")
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("ID token 算法 %q 不受支持（只认 RS256）", header.Alg)
	}
	// 取公钥：缓存命中且未过期直接用，否则重拉一遍再找（轮换后新 kid）
	c.mu.Lock()
	key, ok := c.keys[header.Kid]
	fresh := time.Since(c.fetched) < jwksTTL
	c.mu.Unlock()
	if !ok || !fresh {
		c.mu.Lock()
		err := c.fetch(ctx, cli, jwksURL)
		c.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("拉取 JWKS 失败: %v", err)
		}
		c.mu.Lock()
		key, ok = c.keys[header.Kid]
		c.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("JWKS 里没有 kid %q 对应的公钥", header.Kid)
		}
	}
	// RS256 签名验证（对 ASCII 的 signing input 做 SHA-256 + PKCS#1 v1.5）
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("ID token 签名不是合法 base64url")
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("ID token 签名验证失败")
	}
	// 声明校验：iss / aud / exp / nonce
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("ID token payload 无法解析")
	}
	var claims struct {
		codexIDClaims
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(pb, &claims); err != nil {
		return nil, errors.New("ID token 声明无法解析")
	}
	if claims.Iss != codexIssuer {
		return nil, fmt.Errorf("ID token 签发者 %q 不符", claims.Iss)
	}
	if !codexAudContains(claims.Aud, audience) {
		return nil, fmt.Errorf("ID token 受众与 client_id 不匹配")
	}
	if claims.Exp <= time.Now().Add(-30*time.Second).Unix() {
		return nil, errors.New("ID token 已过期")
	}
	if nonce != "" && claims.Nonce != nonce {
		return nil, errors.New("ID token nonce 不匹配")
	}
	if claims.Subject == "" {
		return nil, errors.New("ID token 缺少 sub")
	}
	out := &claims.codexIDClaims
	return out, nil
}

// codexAudContains aud 兼容字符串与数组两种形态
func codexAudContains(raw json.RawMessage, audience string) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == audience
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		for _, v := range list {
			if v == audience {
				return true
			}
		}
	}
	return false
}

// ── 模型目录与错误分类 ────────────────────────────────────────────────────

// codexFetchModels 拉取账号可见的模型目录。SIWC 走 GET /v1/models（形状是
// models[].slug/display_name/visibility，只保留 visibility == "list" 的条目并
// 保留服务端排序）；CLI 模式没有目录端点，返回内置列表。
func (a *Adapter) fetchModels(ctx context.Context, cli *http.Client, cred *CodexCred) ([]string, error) {
	if codexIsCLI(cred) {
		return append([]string(nil), codexCLIModels...), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.api()+codexModelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cred.AccessToken))
	req.Header.Set("Accept", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("模型目录请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("模型目录返回 HTTP %d: %s", resp.StatusCode, upstream.ErrSnippet(b))
	}
	var v struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
		Data []struct { // 兼容标准 OpenAI /v1/models 形态
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, errors.New("模型目录不是合法 JSON")
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range v.Models {
		slug := strings.TrimSpace(m.Slug)
		if slug == "" || m.Visibility != "list" || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	for _, m := range v.Data { // SIWC 主形状为空时兜底（不应发生，防御性保留）
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("模型目录为空（账号可能没有可用的套餐模型）")
	}
	return out, nil
}

// codexErrorCode 从错误响应体里取 error.code（Responses 层结构化错误）
func codexErrorCode(body []byte) string {
	var v struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.Error.Code
}

// codexUsageLimitExceeded 套餐用量达到上限（429）：按网关 429 语义自动静音。
func codexUsageLimitExceeded(status int, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	return codexErrorCode(body) == "subscription_sharing_usage_limit_exceeded"
}
