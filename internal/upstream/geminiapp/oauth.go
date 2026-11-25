package geminiapp

// Google OAuth：标准回环 PKCE 登录、refresh token 续期、userinfo 身份。
// 不预置任何账号凭证，也不借用本机 Gemini 应用的登录态。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"llmrouter/internal/domain"
)

var (
	// errAuthRevoked refresh token 已不可用（被撤销 / 过期 / 授权码无效）
	errAuthRevoked = errors.New("Google 账号授权已失效或被撤销，请重新登录")
	// errOAuthClient OAuth 客户端本身被拒（client_id / client_secret 不对）
	errOAuthClient = errors.New("OAuth 客户端无效（client_id / client_secret 被 Google 拒绝）")
	// errOAuthClientMissing 没有可用的 OAuth 客户端配置
	errOAuthClientMissing = errors.New("未配置 Google OAuth 客户端：请设置 LLMROUTER_GEMINI_CLIENT_ID / LLMROUTER_GEMINI_CLIENT_SECRET，或在凭证里提供 client_id / client_secret")

	subjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
)

// tokenResponse OAuth 令牌端点的成功响应
type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    float64 `json:"expires_in"`
}

// clientIDFor / clientSecretFor 凭证自带的 OAuth 客户端优先，否则用环境变量配置的
func (a *Adapter) clientIDFor(c *AppCred) string {
	if c != nil && strings.TrimSpace(c.ClientID) != "" {
		return strings.TrimSpace(c.ClientID)
	}
	return a.oauthClientID
}

func (a *Adapter) clientSecretFor(c *AppCred) string {
	if c != nil && strings.TrimSpace(c.ClientSecret) != "" {
		return strings.TrimSpace(c.ClientSecret)
	}
	return a.oauthClientSecret
}

// noRedirectClient 共享传输层的浅拷贝客户端：不跟随重定向（令牌只发给固定端点）
func noRedirectClient(cli *http.Client) *http.Client {
	c := *cli
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// oauthPost POST 令牌端点（换授权码 / 续期共用）。端点的错误体可能含凭证或账号
// 数据，绝不回显：只把白名单里的错误码翻译成可读信息。网络错误与 5xx 重试一次。
func oauthPost(ctx context.Context, cli *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(oauthTransientBackoff)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := noRedirectClient(cli).Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求 Google 令牌端点失败: %w", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var v struct {
			tokenResponse
			Error string `json:"error"`
		}
		if jerr := json.Unmarshal(b, &v); jerr != nil {
			lastErr = fmt.Errorf("Google 令牌端点返回的不是合法 JSON（HTTP %d）", resp.StatusCode)
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 && v.AccessToken != "" {
			return &v.tokenResponse, nil
		} else {
			switch v.Error {
			case "invalid_grant":
				return nil, errAuthRevoked
			case "invalid_client", "unauthorized_client":
				return nil, errOAuthClient
			case "invalid_scope":
				return nil, errors.New("Google 拒绝了请求的授权范围（invalid_scope）")
			case "access_denied":
				return nil, errors.New("账号拒绝了授权（access_denied）")
			}
			lastErr = fmt.Errorf("Google OAuth 令牌端点拒绝（HTTP %d）", resp.StatusCode)
		}
		if resp.StatusCode < 500 {
			break
		}
	}
	return nil, lastErr
}

// exchangeCode 用授权码换 token（回环 PKCE）
func (a *Adapter) exchangeCode(ctx context.Context, cli *http.Client, cred *AppCred, code, verifier, redirectURI string) (*tokenResponse, error) {
	id := a.clientIDFor(cred)
	if id == "" {
		return nil, errOAuthClientMissing
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {id},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	if secret := a.clientSecretFor(cred); secret != "" {
		form.Set("client_secret", secret)
	}
	return oauthPost(ctx, cli, a.tokenURL(), form)
}

// refreshTokens 用 refresh token 续期 access token（带上完整的刷新 scope 集）
func (a *Adapter) refreshTokens(ctx context.Context, cli *http.Client, cred *AppCred) (*tokenResponse, error) {
	id := a.clientIDFor(cred)
	if id == "" {
		return nil, errOAuthClientMissing
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {id},
		"refresh_token": {cred.RefreshToken},
		"scope":         {strings.Join(oauthRefreshScopes, " ")},
	}
	if secret := a.clientSecretFor(cred); secret != "" {
		form.Set("client_secret", secret)
	}
	return oauthPost(ctx, cli, a.tokenURL(), form)
}

// revoke 尽力吊销 Google 侧授权（删除 Provider 时调用）。refresh token 优先：
// 吊销它同时让相关 access token 失效。失败只记日志，不阻断本地删除。
func (a *Adapter) revoke(ctx context.Context, cli *http.Client, cred *AppCred) error {
	token := strings.TrimSpace(cred.RefreshToken)
	if token == "" {
		token = strings.TrimSpace(cred.AccessToken)
	}
	if token == "" {
		return nil
	}
	form := url.Values{
		"token":           {token},
		"token_type_hint": {"refresh_token"},
	}
	if id := a.clientIDFor(cred); id != "" {
		form.Set("client_id", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.revokeURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient(cli).Do(req)
	if err != nil {
		return fmt.Errorf("请求 Google 吊销端点失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Google 吊销端点返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// applyToken 把续期结果并进凭证（保留账号身份等不变字段）
func (a *Adapter) applyToken(cred *AppCred, tr *tokenResponse) *AppCred {
	nc := *cred
	nc.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		nc.RefreshToken = tr.RefreshToken
	}
	seconds := tr.ExpiresIn
	if seconds < 60 {
		seconds = 3600
	}
	exp := a.rt.NowTime().Add(time.Duration(seconds * float64(time.Second)))
	now := a.rt.NowTime()
	nc.ExpiresAt, nc.CheckedAt, nc.AuthError = &exp, &now, ""
	return &nc
}

// refreshToken 续期 access token 并回写。force 时无视临期窗口（401 后强制换新）。
// 同一 provider 的并发续期只发一次上游请求。
func (a *Adapter) refreshToken(p Provider, force bool) (*AppCred, error) {
	lock := a.providerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	cur, ok := a.rt.Store.Provider(p.ID)
	if !ok {
		return nil, errors.New("provider 已删除")
	}
	cred := Cred(cur)
	if cred == nil {
		return nil, errors.New("provider 缺少 Gemini App 凭证")
	}
	if !force && !a.tokenExpiring(cred) {
		return cred, nil
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, errors.New("凭证缺少 refresh token，请重新登录 Google 账号")
	}
	cli, err := a.rt.Clients.For(cur, 20*time.Second)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tr, err := a.refreshTokens(ctx, cli, cred)
	if err != nil {
		if errors.Is(err, errAuthRevoked) { // 授权已死：记下来，后台直接提示重登
			a.rt.Store.UpdateGeminiAppAuthError(p.ID, err.Error())
			return nil, err
		}
		return nil, fmt.Errorf("续期 Gemini App 凭证: %w", err)
	}
	nc := a.applyToken(cred, tr)
	if !a.rt.Store.UpdateGeminiAppToken(p.ID, nc) {
		return nil, errors.New("续期结果未能落盘（provider 可能已删除）")
	}
	return nc, nil
}

// usableToken 取可用于出站的 access token：临期则懒续期；续期失败时手里还有
// 未过期的 token 就先沿用（上游拒了再强制续期）。
func (a *Adapter) usableToken(p Provider) (*AppCred, error) {
	cur, ok := a.rt.Store.Provider(p.ID)
	if !ok {
		return nil, errors.New("provider 已删除")
	}
	cred := Cred(cur)
	if cred == nil {
		return nil, errors.New("provider 缺少 Gemini App 凭证")
	}
	if !a.tokenExpiring(cred) {
		return cred, nil
	}
	nc, err := a.refreshToken(cur, false)
	if err == nil {
		return nc, nil
	}
	if cred.AccessToken != "" && a.rt.NowTime().Before(cred.Expires()) {
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, ProviderID: p.ID,
			Message: "Gemini App 凭证临期续期失败（沿用现有 token）", Detail: err.Error()})
		return cred, nil
	}
	return nil, err
}

// identify 读取账号身份（userinfo）：sub 是账号唯一标识
func (a *Adapter) identify(ctx context.Context, cli *http.Client, accessToken string) (subject, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.userinfoURL(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	resp, err := noRedirectClient(cli).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("读取 Google 账号信息失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", "", errors.New("读取 Google 账号信息失败（userinfo 未返回 200）")
	}
	var v struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(b, &v) != nil || !subjectRe.MatchString(strings.TrimSpace(v.Sub)) {
		return "", "", errors.New("Google 账号信息里缺少有效标识（sub）")
	}
	return strings.TrimSpace(v.Sub), strings.TrimSpace(v.Email), nil
}

// newPKCE 生成 PKCE 的 code_verifier 与 S256 challenge
func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
