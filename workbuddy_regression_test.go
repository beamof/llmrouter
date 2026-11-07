package main

// v0.8.0 评审修复的回归测试：非流式响应头、凭证失效续期、目录缓存与 TTL、
// include_usage 强制、断流错误透出、content:null、零值过期时间、基址强制。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 非流式聚合响应必须是 application/json，且不能沿用上游的 Content-Length
func TestWBNonStreamHeaders(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"\\n\"}}]}\n\n"+ // 节拍帧，过滤后长度变短
			"data: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("非流式聚合响应 Content-Type 应为 application/json，实际 %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		t.Fatalf("非流式响应不应保留 SSE 的 Cache-Control: %q", cc)
	}
	var v struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Object != "chat.completion" {
		t.Fatalf("响应体应是完整 chat.completion: %v: %q", err, body)
	}
	if v.Choices[0].Message.Content != "你" {
		t.Fatalf("节拍过滤后正文应为「你」: %q", body)
	}
}

// 流式响应也不应透传上游 Content-Length（节拍帧被丢弃后长度必然不符）
func TestWBStreamDropsContentLength(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "999") // 与过滤后的实际长度不符
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl == "999" {
		t.Fatalf("不应透传上游 Content-Length: %q", cl)
	}
	if !strings.Contains(body, "你") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("流式内容应完整: %q", body)
	}
}

// 业务码 12153 包在 200 里时也应强制续期并原候选重试
func TestWBTokenExpiredCodeRetries(t *testing.T) {
	var chatCalls atomic.Int32
	var seenAuth []string
	up, refreshes := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		if chatCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":12153,"message":"token invalid"}`) // HTTP 200
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: wbTimePtr(time.Now().Add(time.Hour)), // 未临期，只能由 12153 触发
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("12153 应续期重试后成功: %d %s", resp.StatusCode, body)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("应恰好续期一次，实际 %d", refreshes.Load())
	}
	if len(seenAuth) != 2 || seenAuth[1] != "Bearer at-new" {
		t.Fatalf("重试应使用新 token: %v", seenAuth)
	}
}

// 续期后仍失败时不得无限重试
func TestWBTokenExpiredRetriesOnce(t *testing.T) {
	var chatCalls atomic.Int32
	up, refreshes := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":12153}`)
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: wbTimePtr(time.Now().Add(time.Hour)),
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 502 {
		t.Fatalf("持续失效应 502: %d %s", resp.StatusCode, body)
	}
	if chatCalls.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("应只重试一次（chat=%d refresh=%d）", chatCalls.Load(), refreshes.Load())
	}
}

// 手动续期后应立即用新 token 拉目录
func TestWBRefreshUsesNewTokenForCatalog(t *testing.T) {
	var catalogAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/config", func(w http.ResponseWriter, r *http.Request) {
		catalogAuth = append(catalogAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer at-new" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":12153}`)
			return
		}
		fmt.Fprint(w, wbCatalogBody)
	})
	mux.HandleFunc("POST /v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: wbTimePtr(time.Now().Add(time.Hour)),
	})
	resp, body := postJSON(t, app, "/api/providers/"+p.ID+"/wb-refresh", "{}")
	if resp.StatusCode != 200 {
		t.Fatalf("续期失败: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"remote":true`) {
		t.Fatalf("续期后应拿到远端目录: %s", body)
	}
	if len(catalogAuth) == 0 || catalogAuth[0] != "Bearer at-new" {
		t.Fatalf("目录请求应使用新 token: %v", catalogAuth)
	}
}

// 空目录不得作为有效目录被缓存，也不得盖掉可用的兜底清单
func TestWBEmptyCatalogNotCached(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"models":[]}}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	// 模型测试把空目录抓到手里，但不应写缓存
	s.wbModelTest(httptest.NewRecorder(), Model{Name: "glm-5.0"}, *p, &wbCN, &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	cat := s.wbCatalogFor(*p)
	if cat.Remote || len(cat.IDs) == 0 {
		t.Fatalf("空目录应退回内置清单: remote=%v ids=%d", cat.Remote, len(cat.IDs))
	}
	get, err := app.Client().Get(app.URL + "/api/providers/" + p.ID + "/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != 200 || !strings.Contains(string(b), "minimax-m2.5") {
		t.Fatalf("模型清单应退回内置兜底: %d %s", get.StatusCode, b)
	}
}

// 远端目录应有 TTL，空目录永远不算有效
func TestWBCatalogTTL(t *testing.T) {
	s, _ := newTestServer(t)
	s.wbMu.Lock()
	s.wbCats = map[string]*wbCatalog{"p-cat": {
		Remote: true, IDs: []string{"stale"}, Meta: map[string]*wbModelMeta{},
		At: time.Now().Add(-wbCatalogTTL - time.Minute),
	}}
	s.wbMu.Unlock()
	p := Provider{ID: "p-cat", Kind: "workbuddy", BaseURL: "https://127.0.0.1:1/v2", WB: &WorkBuddyCred{UID: "u", AccessToken: "a"}}
	cat := s.wbCatalogFor(p) // 过期 + 拉取失败 → 内置清单
	if len(cat.IDs) == 1 && cat.IDs[0] == "stale" {
		t.Fatal("过期目录不应继续使用")
	}
	if !wbUseCatalog(&wbCatalog{Remote: true, IDs: []string{"a"}, At: time.Now()}) {
		t.Fatal("新拉的远端目录应在 TTL 内有效")
	}
	if wbUseCatalog(&wbCatalog{Remote: true, IDs: []string{"a"}, At: time.Now().Add(-wbCatalogTTL - time.Minute)}) {
		t.Fatal("超出 TTL 的远端目录应失效")
	}
	if wbUseCatalog(&wbCatalog{Remote: true, IDs: nil}) {
		t.Fatal("空目录不应视为有效目录")
	}
}

// 客户端传 include_usage:false 也要强制 true，否则流式用量记不到
func TestWBForceIncludeUsage(t *testing.T) {
	m := map[string]any{"stream_options": map[string]any{"include_usage": false, "other": 1}}
	wbEnsureStreamOptions(m)
	so := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("应按网关需要强制 include_usage: %#v", so)
	}
	if so["other"] != 1 {
		t.Fatalf("其余 stream_options 字段应保留: %#v", so)
	}
}

// 上游断流应作为错误返回（非流式），不能产出"成功"的截断响应
func TestWBTruncatedStreamFailsOver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/config":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, wbCatalogBody)
		case "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			// 声明 1KB 却只写一段就断开：客户端读取会拿到 unexpected EOF
			w.Header().Set("Content-Length", "1024")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n")
		}
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"glm-5.0","choices":[{"message":{"role":"assistant","content":"备用"}}]}`)
	}))
	defer fallback.Close()
	addProviderModel(t, s, fallback.URL, "glm-5.0", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "备用") {
		t.Fatalf("断流应故障转移到下一候选: %d %s", resp.StatusCode, body)
	}
}

// 纯 tool_calls 响应的 content 应为 null（而非缺字段）
func TestWBAggToolCallContentNull(t *testing.T) {
	agg := &wbAgg{}
	agg.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"fx\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"))
	body, _ := agg.result("glm-5.0")
	var v struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Choices[0].Message.Content != nil {
		t.Fatalf("纯 tool_calls 时 content 应为 null: %s", body)
	}
}

// 未设置过期时间时，凭证不应输出 0001-01-01（muted_until 是另一个既有字段，不在此断言）
func TestWBZeroExpiryOmitted(t *testing.T) {
	s, _ := newTestServer(t)
	addWBProviderModel(t, s, "https://example.invalid", "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	var v struct {
		Providers []struct {
			WB map[string]any `json:"wb"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(s.store.SnapshotJSON(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Providers) != 1 {
		t.Fatalf("应有一个 provider: %s", s.store.SnapshotJSON())
	}
	if _, ok := v.Providers[0].WB["expires_at"]; ok {
		t.Fatalf("零值过期时间应省略，实际 %v", v.Providers[0].WB["expires_at"])
	}
}

// WorkBuddy provider 的基址必须由地区端点决定，不接受自定义（防凭证外发）
func TestWBProviderBaseURLForced(t *testing.T) {
	s, app := newTestServer(t)
	resp, body := postJSON(t, app, "/api/providers",
		`{"name":"wb","kind":"workbuddy","base_url":"https://evil.example.com/v2","wb":{"uid":"u","access_token":"a"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("创建失败: %d %s", resp.StatusCode, body)
	}
	p, _ := s.store.ProviderByWBUID("workbuddy", "u")
	if p.BaseURL != "https://www.workbuddy.cn/v2" {
		t.Fatalf("基址应被强制为官方端点，实际 %q", p.BaseURL)
	}
}

// 删除 Provider 应顺带清掉续期锁与目录缓存
func TestWBForgetProviderOnDelete(t *testing.T) {
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, "https://example.invalid/v2", "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	s.wbCatalogFor(*p) // 生成目录缓存（内置兜底）
	_ = s.wbProviderLock(p.ID)
	req, _ := http.NewRequest("DELETE", app.URL+"/api/providers/"+p.ID, nil)
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("删除失败: %d", resp.StatusCode)
	}
	s.wbMu.Lock()
	_, hasLock := s.wbLocks[p.ID]
	_, hasCat := s.wbCats[p.ID]
	s.wbMu.Unlock()
	if hasLock || hasCat {
		t.Fatalf("删除 provider 后应清掉 wbLocks/wbCats: lock=%v cat=%v", hasLock, hasCat)
	}
}
