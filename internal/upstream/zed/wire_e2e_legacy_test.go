//go:build legacy

package zed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── 信封装配 ───────────────────────────────────────────────────────────────

func zedSetupProxy(t *testing.T, model string, completions http.HandlerFunc) (*zedFakeCloud, *Server, *httptest.Server) {
	t.Helper()
	fc, s, app := zedStartFakeCloud(t)
	fc.handler = completions
	cred := &ZedCred{
		UserID: "user-1", NativeCredential: json.RawMessage(`{"a":1}`),
		AccessToken: fc.jwt, ExpiresAt: timePtr(time.Now().Add(time.Hour)),
	}
	s.rt.Store.UpsertProvider(&Provider{Kind: "zed", BaseURL: fc.base + "/completions", Zed: cred})
	s.rt.Store.UpsertModel(&Model{Name: model, ProviderID: "p1", Priority: 10})
	return fc, s, app
}

func timePtr(t time.Time) *time.Time { return &t }

func TestZedProxyChatToClaude(t *testing.T) {
	var gotEnvelope map[string]any
	var gotAuth string
	_, s, app := zedSetupProxy(t, "claude-sonnet-4.5", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotEnvelope)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, zedAnthropicSSE)
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("上游鉴权头: %q", gotAuth)
	}
	if gotEnvelope["provider"] != zedProviderAnthropic || gotEnvelope["model"] != "claude-sonnet-4.5" {
		t.Fatalf("信封: %s", mustJSON(gotEnvelope))
	}
	pr := zedMap(gotEnvelope["provider_request"])
	if _, has := pr["stream"]; has || zedStr(zedMap(zedSlice(zedMap(zedSlice(pr["messages"])[0])["content"])[0])["text"]) != "hi" {
		t.Fatalf("provider_request: %s", mustJSON(pr))
	}
	res := zedParse(t, body)
	if res["object"] != "chat.completion" {
		t.Fatalf("客户端应收到 chat.completion: %s", body)
	}
	// usage 落盘
	us := s.rt.Store
	_ = us
	if len(s.rt.Store.Usage) == 0 || s.rt.Store.Usage[0].PromptTokens != 5 || s.rt.Store.Usage[0].CompletionTokens != 3 {
		t.Fatalf("用量未记账: %s", mustJSON(s.rt.Store.Usage))
	}
}

func TestZedProxyChatStreamToGPT(t *testing.T) {
	_, _, app := zedSetupProxy(t, "gpt-5.4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, zedResponsesSSE)
	})
	resp, err := app.Client().Post(app.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(b)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("流式头: %s", ct)
	}
	for _, want := range []string{`"content":"嗨"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
}

func TestZedProxyResponsesToGPTPassthrough(t *testing.T) {
	var gotEnvelope map[string]any
	_, _, app := zedSetupProxy(t, "gpt-5.4", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotEnvelope)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, zedResponsesSSE)
	})
	resp, body := postJSON(t, app, "/v1/responses",
		`{"model":"gpt-5.4","stream":true,"instructions":"codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	pr := zedMap(gotEnvelope["provider_request"])
	if pr["instructions"] != "codex" {
		t.Fatalf("instructions 应保真: %s", mustJSON(pr))
	}
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"id":"resp_1"`) {
		t.Fatalf("responses 客户端应收到事件流: %s", body)
	}
}

func TestZedProxyResponsesToClaude(t *testing.T) {
	_, _, app := zedSetupProxy(t, "claude-sonnet-4.5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, zedAnthropicSSE)
	})
	resp, body := postJSON(t, app, "/v1/responses",
		`{"model":"claude-sonnet-4.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	res := zedParse(t, body)
	if res["object"] != "response" {
		t.Fatalf("应聚合为 response 对象: %s", body)
	}
	found := false
	for _, raw := range zedSlice(res["output"]) {
		if item := zedMap(raw); zedStr(item["type"]) == "message" &&
			strings.Contains(mustJSON(item), "好") {
			found = true
		}
	}
	if !found {
		t.Fatalf("output 应含翻译文本: %s", body)
	}
}

// 401 → 强制重铸 → 原候选重试一次

func TestZedProxyRefreshOn401(t *testing.T) {
	var fc *zedFakeCloud // 先声明，闭包按引用捕获（调用时已完成赋值）
	fc, s, app := zedSetupProxy(t, "claude-sonnet-4.5", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fc.jwt {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"token expired"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, zedAnthropicSSE)
	})
	// 把 provider 里的 JWT 换成过期值，逼出 401 → 重铸（伪造云端返回 fc.jwt）→ 重试成功
	p, _ := s.rt.Store.ProviderByZedUser("user-1")
	stale := *p.Zed
	stale.AccessToken = "stale-token"
	stale.ExpiresAt = timePtr(time.Now().Add(-time.Minute))
	s.rt.Store.UpdateZedAuth(p.ID, &stale)

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("重铸重试后应成功: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"content":"好"`) {
		t.Fatalf("应收到内容: %s", body)
	}
	after, _ := s.rt.Store.ProviderByZedUser("user-1")
	if after.Zed.AccessToken != fc.jwt {
		t.Fatal("重铸后的 JWT 应落盘")
	}
}

// 403 含 plan → 只静音该模型（1 小时），渠道仍在

func TestZedProxyPlanMute(t *testing.T) {
	_, s, app := zedSetupProxy(t, "claude-sonnet-4.5", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"message":"your plan does not include this model"}}`)
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 502 {
		t.Fatalf("套餐拒绝应失败给客户端: %d %s", resp.StatusCode, body)
	}
	m := s.rt.Store.Models[0]
	if m.MutedUntil.IsZero() || !m.MutedUntil.After(time.Now()) {
		t.Fatalf("模型应被静音: %+v", m)
	}
	if m.MuteReason == "" || !strings.Contains(m.MuteReason, "套餐") {
		t.Fatalf("静音原因: %q", m.MuteReason)
	}
}
