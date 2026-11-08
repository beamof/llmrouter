package main

// 网络代理测试：地址规范化、Transport 缓存、后台校验与端到端 HTTP 代理转发。

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  string
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "127.0.0.1:7890", want: "http://127.0.0.1:7890"},                                  // 裸地址按 http 补全
		{in: "http://127.0.0.1:7890", want: "http://127.0.0.1:7890"},                           //
		{in: " http://user:pw@proxy.example:8080 ", want: "http://user:pw@proxy.example:8080"}, // 带认证
		{in: "SOCKS5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},                       // scheme 大小写归一
		{in: "socks5h://127.0.0.1:1080", want: "socks5h://127.0.0.1:1080"},
		{in: "https://proxy.example", want: "https://proxy.example"},
		{in: "ftp://127.0.0.1:21", wantErr: "不受支持"},
		{in: "http://", wantErr: "缺少主机"},
	}
	for _, c := range cases {
		got, err := normalizeProxy(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("normalizeProxy(%q) 应报错含 %q，得到 %v", c.in, c.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeProxy(%q) 不应报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeProxy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUpstreamClient(t *testing.T) {
	s, _ := newTestServer(t)
	// 未配置代理：用共享客户端的 Transport，且带整体超时
	cli, err := s.upstreamClient(Provider{BaseURL: "https://x.example/v1"}, 15*time.Second)
	if err != nil {
		t.Fatalf("无代理不应报错: %v", err)
	}
	if cli.Transport != s.client.Transport {
		t.Errorf("无代理应沿用共享 Transport")
	}
	if cli.Timeout != 15*time.Second {
		t.Errorf("Timeout = %v, want 15s", cli.Timeout)
	}
	// 非法代理 fail-closed：报错而不是回退直连
	if _, err := s.upstreamClient(Provider{Proxy: "ftp://x"}, 0); err == nil {
		t.Errorf("非法代理应报错")
	}
	// 同一代理地址复用同一 Transport（连接池共享），不同地址各自一份
	p1 := Provider{Proxy: "http://127.0.0.1:7890"}
	p2 := Provider{Proxy: "127.0.0.1:7890"} // 规范化后与 p1 同址
	p3 := Provider{Proxy: "socks5://127.0.0.1:1080"}
	c1, _ := s.upstreamClient(p1, 0)
	c2, _ := s.upstreamClient(p2, 0)
	c3, _ := s.upstreamClient(p3, 0)
	if c1.Transport == s.client.Transport {
		t.Errorf("配置代理后不应沿用共享 Transport")
	}
	if c1.Transport != c2.Transport {
		t.Errorf("规范化后同址的代理应复用同一 Transport")
	}
	if c1.Transport == c3.Transport {
		t.Errorf("不同代理地址不应共享 Transport")
	}
}

func TestProviderUpsertProxyValidation(t *testing.T) {
	_, app := newTestServer(t)
	// 非法 scheme 拒绝
	resp, body := postJSON(t, app, "/api/providers", `{"name":"bad","base_url":"https://x.example/v1","proxy":"ftp://1.2.3.4:21"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 proxy 应 400，得到 %d: %s", resp.StatusCode, body)
	}
	// 裸地址自动补 http:// 落盘
	resp, body = postJSON(t, app, "/api/providers", `{"name":"ok","base_url":"https://x.example/v1","proxy":"127.0.0.1:7890"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 proxy 应 200，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"proxy":"http://127.0.0.1:7890"`) {
		t.Errorf("落盘应存规范化后的代理地址: %s", body)
	}
}

// TestChatThroughHTTPProxy 端到端：provider 配了 http 代理后，对话转发的
// 上游请求必须经代理发出（代理收到绝对地址形式的请求并转发到上游）。
func TestChatThroughHTTPProxy(t *testing.T) {
	var upstreamHits, proxyHits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"m1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	// 极简 HTTP 正向代理：收到绝对地址请求后原样转发（http 目标不会出现 CONNECT）
	direct := &http.Client{Transport: &http.Transport{}} // 内层直连，避免再吃环境变量代理
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if !strings.HasPrefix(r.RequestURI, "http://") {
			http.Error(w, "代理应收到绝对地址请求，得到 "+r.RequestURI, http.StatusBadRequest)
			return
		}
		resp, err := direct.Do(&http.Request{Method: r.Method, URL: r.URL, Header: r.Header, Body: r.Body})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	s, app := newTestServer(t)
	p := &Provider{Name: "m1-p", BaseURL: up.URL, APIKey: "sk-up", Proxy: proxy.URL}
	s.store.UpsertProvider(p)
	s.store.UpsertModel(&Model{Name: "m1", ProviderID: p.ID, Priority: 10})
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", resp.StatusCode, body)
	}
	if n := proxyHits.Load(); n != 1 {
		t.Errorf("代理应收到 1 次请求，得到 %d", n)
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Errorf("上游应收到 1 次请求，得到 %d", n)
	}
	// provider 配置非法代理时 fail-closed：不直连上游，返回错误
	p.Proxy = "http://" // 缺主机
	s.store.UpsertProvider(p)
	resp, body = postJSON(t, app, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("非法代理应失败换候选最终 502，得到 %d: %s", resp.StatusCode, body)
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Errorf("非法代理不应绕过代理直连上游（上游命中 %d 次）", n)
	}
}
