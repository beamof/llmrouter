package main

// 活跃会话列表（GET /api/sessions）：绑定、计数、auto 标记、排序与过期剔除。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestActiveSessionsListed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	p := &Provider{Name: "P", BaseURL: up.URL}
	s.store.UpsertProvider(p)
	s.store.UpsertModel(&Model{Name: "glm", ProviderID: p.ID})
	modelID := s.store.Models[0].ID

	fetch := func() []sessionInfo {
		t.Helper()
		resp, err := app.Client().Get(app.URL + "/api/sessions")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("sessions 应 200: %d %s", resp.StatusCode, b)
		}
		var v struct {
			Sessions []sessionInfo `json:"sessions"`
		}
		if json.Unmarshal(b, &v) != nil {
			t.Fatalf("响应不是合法 JSON: %s", b)
		}
		return v.Sessions
	}

	if got := fetch(); len(got) != 0 {
		t.Fatalf("初始应无活跃会话: %#v", got)
	}
	// conv-a 两次 + conv-b 一次；最后活跃的是 conv-b（列表应排最前）
	for _, sid := range []string{"conv-a", "conv-a", "conv-b"} {
		postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm"}`, "X-Session-ID", sid)
		// Windows 的时钟粒度约 1ms，连续请求可能取到相同的 LastAt 导致排序断言不稳定
		time.Sleep(5 * time.Millisecond)
	}
	got := fetch()
	if len(got) != 2 {
		t.Fatalf("应有 2 个活跃会话: %#v", got)
	}
	if got[0].ID != "conv-b" || got[1].ID != "conv-a" {
		t.Fatalf("应按最近活跃倒序: %#v", got)
	}
	a := got[1]
	if a.ModelID != modelID || a.Requested != "glm" || a.Count != 2 {
		t.Fatalf("conv-a 字段不对: %#v", a)
	}
	if a.FirstAt.IsZero() || a.LastAt.IsZero() || !a.Expires.After(a.LastAt) {
		t.Fatalf("时刻字段不对: %#v", a)
	}

	// 过期会话不出现（顺带被清理）
	s.sessMu.Lock()
	s.sessions["dead"] = session{modelID: "x", expires: time.Now().Add(-time.Second)}
	s.sessMu.Unlock()
	if got := fetch(); len(got) != 2 {
		t.Fatalf("过期会话不应出现: %#v", got)
	}
}
