package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmrouter/internal/upstream/codex"
)

// TestCodexProviderWiring Codex 套餐 provider 的后台接线：kind 校验、
// 基址固定、注册表分发与登录路由挂载。
func TestCodexProviderWiring(t *testing.T) {
	s, app := newTestServer(t)

	// 缺凭证的 codex provider 应被拒绝
	resp, body := postJSON(t, app, "/api/providers", `{"name":"codex1","kind":"codex","base_url":"x"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "codex") {
		t.Fatalf("缺凭证应被拒绝: %d %s", resp.StatusCode, body)
	}

	// 带凭证创建：基址由适配器固定为官方 responses 端点
	exp := time.Now().Add(time.Hour)
	cred, _ := json.Marshal(map[string]any{
		"subject": "auth0|u1", "client_id": "oaiapp_x", "access_token": "at",
		"refresh_token": "rt", "expires_at": exp.Format(time.RFC3339),
	})
	resp, body = postJSON(t, app, "/api/providers", `{"name":"codex1","kind":"codex","codex":`+string(cred)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建 codex provider 失败: %d %s", resp.StatusCode, body)
	}
	p := s.store.Providers[len(s.store.Providers)-1]
	if p.Kind != "codex" || p.BaseURL != "https://api.openai.com/v1/responses" {
		t.Fatalf("provider 形态: %+v", p)
	}
	// 注册表分发到 codex 适配器
	if _, ok := s.registry().For(*p).(*codex.Adapter); !ok {
		t.Fatalf("应分发到 codex 适配器: %T", s.registry().For(*p))
	}
	if !s.registry().For(*p).CanServe("chat") || !s.registry().For(*p).CanServe("responses") {
		t.Fatal("codex 适配器应承接两种接入协议")
	}

	// 登录路由已挂载（handler 自身的 404/400 说明路由存在；Go mux 缺路由才回 "404 page not found"）
	req, _ := http.NewRequest(http.MethodGet, app.URL+"/api/codex/login/status?state=x", nil)
	if r, err := app.Client().Do(req); err != nil {
		t.Fatalf("登录状态路由应存在: %v", err)
	} else if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), "404 page not found") {
		t.Fatalf("登录状态路由未挂载: %s", b)
	}
	req, _ = http.NewRequest(http.MethodPost, app.URL+"/api/providers/"+p.ID+"/codex-refresh", nil)
	if r, err := app.Client().Do(req); err != nil {
		t.Fatalf("codex-refresh 路由应存在: %v", err)
	} else if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), "404 page not found") {
		t.Fatalf("codex-refresh 路由未挂载: %s", b)
	}

	// store 的 codex 辅助方法
	if old, ok := s.store.ProviderByCodexSubject("auth0|u1", ""); !ok || old.ID != p.ID {
		t.Fatal("ProviderByCodexSubject 应命中")
	}
	if !s.store.UpdateCodexAuth(p.ID, p.Codex) {
		t.Fatal("UpdateCodexAuth 应成功")
	}
}

// TestCodexHostIDPersistedAcrossServerRestart host ID 落盘后重启仍是同一个
func TestCodexHostIDPersistedAcrossServerRestart(t *testing.T) {
	s, _ := newTestServer(t)
	id1 := s.store.EnsureCodexHostID()
	path := s.store.Path()
	s.Shutdown() // 落盘

	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.EnsureCodexHostID() != id1 {
		t.Fatalf("重启后 host ID 应不变: %q", id1)
	}
}

// TestCodexAttemptCompat codexAttempt 兼容入口可以透传调用（冒烟）
func TestCodexAttemptCompat(t *testing.T) {
	s, _ := newTestServer(t)
	exp := time.Now().Add(time.Hour)
	p := &Provider{ID: "p1", Name: "codex1", Kind: "codex", BaseURL: "http://127.0.0.1:1/responses",
		Codex: &CodexCred{Subject: "s", ClientID: "c", AccessToken: "at", RefreshToken: "rt", ExpiresAt: &exp}}
	s.store.UpsertProvider(p)
	s.store.UpsertModel(&Model{ID: "m1", ProviderID: "p1", Name: "gpt-6.1-sol"})
	c := Candidate{Provider: *p, Model: Model{ID: "m1", ProviderID: "p1", Name: "gpt-6.1-sol"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`))
	err := s.codexAttempt(rec, req, c, []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`), "k", false, nil, nil)
	if err == nil {
		t.Fatal("不可达上游应失败")
	}
}
