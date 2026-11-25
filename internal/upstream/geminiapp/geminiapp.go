// Package geminiapp 实现 Gemini App（Google 账号订阅）上游适配器：Google OAuth
// 回环登录、token 续期、原生 HTTP/2 gRPC 线协议、提示词工具协议，以及 chat /
// responses 两种接入协议的回程翻译。
//
// 复用 Gemini 网页/App 订阅额度的通道不是公开 API：请求走 Gemini 原生 Robin 服务
// （assistant.mobile.robin.RobinService/ProcessQuery），请求体是手写的 protobuf，
// 回程是 gRPC 分帧的整文快照流。该后端没有可供第三方使用的函数调用通道，工具
// 调用改走「提示词协议」：工具声明渲染进提示词，模型在可见回复里用 ```tool_call
// 围栏请求调用，网关解析并校验后才翻译成 tool_calls 交给客户端。
//
// 线协议、提示词文本、OAuth 参数与错误分类对齐开源实现 gemini-app2api（MIT）；
// 账号池、自动重试恢复等由本网关的路由组与候选循环承担，这里只实现单账号单回合。
package geminiapp

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
)

// Kind Provider.Kind 取值。
const Kind = "geminiapp"

const (
	nativeOrigin     = "https://robinfrontend-pa.googleapis.com"
	processQueryPath = "/assistant.mobile.robin.RobinService/ProcessQuery"
	usageInfoPath    = "/assistant.mobile.robin.RobinService/GetUsageInfo"
	termsVersionPath = "/assistant.mobile.robin.RobinService/GetTermsOfServiceVersion"
	termsAcceptPath  = "/assistant.mobile.robin.RobinService/AcceptTermsOfService"
	uploadOrigin     = "https://push.clients6.google.com"

	// nativeVersion ProcessQuery 的兼容版本，与请求元数据里的客户端版本一致
	nativeVersion = "1.135.1.1291"
	// gatewayUserAgent 出站 User-Agent（线协议常量，随上游演进更新）
	gatewayUserAgent = "gemini-app2api/1.0"
	// nativeClientType 请求元数据里的客户端类型
	nativeClientType = 21

	oauthAuthURL     = "https://accounts.google.com/o/oauth2/auth"
	oauthTokenURL    = "https://www.googleapis.com/oauth2/v4/token"
	oauthUserinfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"
	oauthRevokeURL   = "https://oauth2.googleapis.com/revoke"

	// oauthClientIDEnv / oauthClientSecretEnv 登录与续期使用的 Google OAuth 客户端。
	// 仓库不内置客户端凭证；也可由 Provider 凭证自带的 client_id / client_secret 覆盖。
	oauthClientIDEnv     = "LLMROUTER_GEMINI_CLIENT_ID"
	oauthClientSecretEnv = "LLMROUTER_GEMINI_CLIENT_SECRET"
	// oauthLoginScope 登录时征求的 scope；续期时再带上完整的刷新 scope 集
	oauthLoginScope = "https://www.googleapis.com/auth/flexible-api"

	defaultOAuthPort      = 18765
	defaultTurnTimeout    = 180 * time.Second
	oauthCallbackTimeout  = 10 * time.Minute
	oauthRefreshWindow    = 60 * time.Second
	oauthTransientBackoff = 150 * time.Millisecond
)

// oauthRefreshScopes 续期时请求的 scope 集（原生客户端的完整授权范围）
var oauthRefreshScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/assistant",
	"https://www.googleapis.com/auth/client_channel",
	"https://www.googleapis.com/auth/gemini",
	"https://www.googleapis.com/auth/calendar",
	"https://www.googleapis.com/auth/drive",
	"https://www.googleapis.com/auth/photos.image.readonly",
	"https://www.googleapis.com/auth/photospicker.mediaitems.readonly",
	"https://www.googleapis.com/auth/oauth_integrations",
	"https://www.googleapis.com/auth/experimentsandconfigs",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/supportcontent",
	"https://www.googleapis.com/auth/mobile_user_preferences",
	"https://www.googleapis.com/auth/notifications",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/aicode",
}

// 本包内部沿用领域模型的短名称。
type (
	Provider  = domain.Provider
	Model     = domain.Model
	Candidate = domain.Candidate
	AppCred   = domain.GeminiAppCred
	Usage     = domain.Usage
	LogEntry  = domain.LogEntry
)

// Adapter Gemini App 上游适配器。
type Adapter struct {
	rt *upstream.Runtime

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	loginMu sync.Mutex
	logins  map[string]*loginSession

	oauthBase  string // OAuth 授权 / 令牌 / userinfo / 吊销端点基址覆盖（测试注入；空 = 官方端点）
	origin     string // Robin 服务 origin 覆盖（测试注入；空 = 官方端点）
	uploadBase string // 图片上传 origin 覆盖（测试注入；空 = 官方端点）

	// OAuth 客户端：仓库不内置凭证，由环境变量或 Provider 凭证提供。
	oauthClientID     string
	oauthClientSecret string

	turnTimeout        time.Duration // 单个回合的总超时
	postCompleteGrace  time.Duration // 上游标记完成后等待关流的宽限
	postOutputIdle     time.Duration // 正文停止推进后的空闲上限（未声明工具）
	postOutputToolIdle time.Duration // 同上；声明了工具时给可选的工具调用留更久
	postToolIdle       time.Duration // 工具调用发出后的空闲上限
	oauthPort          int           // 登录回调端口
}

// New 创建适配器。
func New(rt *upstream.Runtime) *Adapter {
	a := &Adapter{
		rt: rt, locks: map[string]*sync.Mutex{}, logins: map[string]*loginSession{},

		turnTimeout:        defaultTurnTimeout,
		postCompleteGrace:  2 * time.Second,
		postOutputIdle:     15 * time.Second,
		postOutputToolIdle: 45 * time.Second,
		postToolIdle:       6 * time.Second,
		oauthPort:          defaultOAuthPort,
	}
	// 回合总超时可调：重度思考任务可能超过默认的 3 分钟（上限 10 分钟）
	if v, err := strconv.Atoi(os.Getenv("LLMROUTER_GEMINI_TURN_TIMEOUT")); err == nil && v >= 1 && v <= 600 {
		a.turnTimeout = time.Duration(v) * time.Second
	}
	// 登录回调端口固定时便于无浏览器环境做端口转发；填 0 每次随机，被占用时自动改用随机端口
	if v, err := strconv.Atoi(os.Getenv("LLMROUTER_GEMINI_OAUTH_PORT")); err == nil && v >= 0 && v <= 65535 {
		a.oauthPort = v
	}
	a.oauthClientID = strings.TrimSpace(os.Getenv(oauthClientIDEnv))
	a.oauthClientSecret = strings.TrimSpace(os.Getenv(oauthClientSecretEnv))
	return a
}

// NewWithBases 创建适配器并覆盖 OAuth / Robin / 上传端点（测试注入用）。
func NewWithBases(rt *upstream.Runtime, oauthBase, origin, uploadBase string) *Adapter {
	a := New(rt)
	a.oauthBase, a.origin, a.uploadBase = oauthBase, origin, uploadBase
	return a
}

// Kinds 接管的 Provider.Kind。
func (a *Adapter) Kinds() []string { return []string{Kind} }

// CanServe 适配层两种接入协议都承接（出站统一折成提示词）。
func (a *Adapter) CanServe(domain.Protocol) bool { return true }

// ValidateProvider 固定数据面基址并校验凭证（基址固定为官方端点，避免把
// 凭证发往其他主机）。
func (a *Adapter) ValidateProvider(p *domain.Provider) error {
	if p.GeminiApp == nil || strings.TrimSpace(p.GeminiApp.Subject) == "" {
		return errors.New("Gemini App 类型的 provider 必须携带凭证（geminiapp.subject），编辑时请勿丢弃 geminiapp 字段")
	}
	p.BaseURL = a.robinOrigin() + processQueryPath
	p.ResponsesURL = ""
	return nil
}

// Forget 释放该 Provider 的续期单飞锁，并尽力吊销 Google 侧授权。
// 吊销失败只记日志：用户也可以在 Google 账号设置里手动移除访问权限。
func (a *Adapter) Forget(id string) {
	a.mu.Lock()
	delete(a.locks, id)
	a.mu.Unlock()
	p, ok := a.rt.Store.Provider(id)
	if !ok {
		return
	}
	cred := Cred(p)
	if cred == nil || (strings.TrimSpace(cred.RefreshToken) == "" && strings.TrimSpace(cred.AccessToken) == "") {
		return
	}
	cli, err := a.rt.Clients.For(p, 10*time.Second)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.revoke(ctx, cli, cred); err != nil {
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, ProviderID: id,
			Message: "Gemini App 授权吊销失败（可在 Google 账号设置里手动移除）", Detail: err.Error()})
		return
	}
	a.rt.Logf(LogEntry{Level: "info", Kind: domain.KindUpstream, ProviderID: id,
		Message: "Gemini App 授权已吊销（Provider 删除）"})
}

// Cred 取 Gemini App 凭证（Kind 不匹配时返回 nil）。
func Cred(p Provider) *AppCred {
	if p.Kind == Kind {
		return p.GeminiApp
	}
	return nil
}

// robinOrigin Robin 服务 origin（测试可注入 origin）。
func (a *Adapter) robinOrigin() string {
	if a.origin != "" {
		return strings.TrimRight(a.origin, "/")
	}
	return nativeOrigin
}

// uploadsOrigin 图片上传 origin（测试可注入 uploadBase）。
func (a *Adapter) uploadsOrigin() string {
	if a.uploadBase != "" {
		return strings.TrimRight(a.uploadBase, "/")
	}
	return uploadOrigin
}

// tokenURL OAuth 令牌端点（oauthBase 可注入）。
func (a *Adapter) tokenURL() string {
	if a.oauthBase != "" {
		return strings.TrimRight(a.oauthBase, "/") + "/token"
	}
	return oauthTokenURL
}

// userinfoURL 账号身份端点（oauthBase 可注入）。
func (a *Adapter) userinfoURL() string {
	if a.oauthBase != "" {
		return strings.TrimRight(a.oauthBase, "/") + "/userinfo"
	}
	return oauthUserinfoURL
}

// authURL 浏览器授权端点（oauthBase 可注入）。
func (a *Adapter) authURL() string {
	if a.oauthBase != "" {
		return strings.TrimRight(a.oauthBase, "/") + "/auth"
	}
	return oauthAuthURL
}

// revokeURL 令牌吊销端点（oauthBase 可注入）。
func (a *Adapter) revokeURL() string {
	if a.oauthBase != "" {
		return strings.TrimRight(a.oauthBase, "/") + "/revoke"
	}
	return oauthRevokeURL
}

// providerLock 续期单飞锁：同一 provider 的并发续期只发一次上游请求
func (a *Adapter) providerLock(id string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.locks == nil {
		a.locks = map[string]*sync.Mutex{}
	}
	l, ok := a.locks[id]
	if !ok {
		l = &sync.Mutex{}
		a.locks[id] = l
	}
	return l
}

// tokenExpiring access token 是否临期（没有 token 或过期时刻未知时视为临期）
func (a *Adapter) tokenExpiring(c *AppCred) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	return a.rt.NowTime().Add(oauthRefreshWindow).After(c.Expires())
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
