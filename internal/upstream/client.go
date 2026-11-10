package upstream

// ─────────────── 按上游 Provider 配置的网络代理 ───────────────
//
// Provider 可各自配置代理地址（http/https/socks5/socks5h）。socks5 与 socks5h
// 均由标准库 net/http 原生支持（域名交由代理解析，即 socks5h 语义），无需额外
// 依赖。代理 Transport 按规范化后的地址缓存复用（连接池共享）；同一地址只建
// 一份。地址非法时宁可让该次请求失败也不绕过代理直连，避免把本应走代理的
// 上游流量（或凭据）从真实出口泄漏出去。

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"llmrouter/internal/domain"
)

// proxySchemes 允许的代理 scheme（与标准库 http.Transport 支持的一致）
var proxySchemes = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// NormalizeProxy 规范化代理地址：去掉首尾空白；无 scheme 时按 http:// 补全
// （裸写 127.0.0.1:7890 的常见习惯）；校验 scheme 与主机非空。空串原样返回。
func NormalizeProxy(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("代理地址无法解析: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !proxySchemes[scheme] {
		return "", fmt.Errorf("代理 scheme %q 不受支持（支持 http/https/socks5/socks5h）", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("代理地址缺少主机与端口")
	}
	u.Scheme = scheme
	return u.String(), nil
}

// ClientFactory 构造访问上游用的 HTTP 客户端，并按代理地址缓存 Transport。
type ClientFactory struct {
	base *http.Client

	mu   sync.Mutex
	tpts map[string]*http.Transport
}

// NewClientFactory 基于共享客户端创建工厂（base 的 Transport 用于直连）。
func NewClientFactory(base *http.Client) *ClientFactory {
	if base == nil {
		base = &http.Client{}
	}
	return &ClientFactory{base: base, tpts: map[string]*http.Transport{}}
}

// proxyTransport 取该代理地址的 Transport（缓存复用，连接池共享）。
// 调用方须先经 NormalizeProxy 校验，这里到不了非法地址。
func (f *ClientFactory) proxyTransport(proxyURL string) *http.Transport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tpts == nil {
		f.tpts = map[string]*http.Transport{}
	}
	if t, ok := f.tpts[proxyURL]; ok {
		return t
	}
	u, _ := url.Parse(proxyURL) // NormalizeProxy 已保证合法
	t := &http.Transport{
		Proxy:               http.ProxyURL(u),
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	f.tpts[proxyURL] = t
	return t
}

// For 构造访问该 Provider 上游用的 HTTP 客户端：
//   - 配置了代理：走按地址缓存的 Transport（该 Provider 所有出站共用同一代理）；
//   - 未配置代理：用共享客户端的 Transport（默认仍读 HTTP(S)_PROXY 环境变量）。
//
// timeout 为 0 表示不设整体超时（流式转发依赖请求级 context，不能用客户端级超时）。
// 代理地址非法时返回错误（fail-closed，不静默直连）。
func (f *ClientFactory) For(p domain.Provider, timeout time.Duration) (*http.Client, error) {
	raw := strings.TrimSpace(p.Proxy)
	if raw == "" {
		return &http.Client{Transport: f.base.Transport, Timeout: timeout}, nil
	}
	proxyURL, err := NormalizeProxy(raw)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: f.proxyTransport(proxyURL), Timeout: timeout}, nil
}
