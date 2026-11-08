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

// 会话 TTL 可配置：设置后新绑定按该时长过期，过期后不再出现在活跃会话列表。
func TestSessionTTLConfigurable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "glm", nil)

	s.store.SetSessionTTL(1) // 1 秒
	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm"}`, "X-Session-ID", "ttl-1")

	s.sessMu.Lock()
	var sess session
	for _, v := range s.sessions {
		sess = v
	}
	s.sessMu.Unlock()
	if got := sess.expires.Sub(sess.lastAt); got != time.Second {
		t.Fatalf("绑定有效期应为 1s，got %v", got)
	}

	time.Sleep(1200 * time.Millisecond)
	resp, err := app.Client().Get(app.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Sessions) != 0 {
		t.Fatalf("过期会话应被清理: %#v", v.Sessions)
	}
	// <=0 恢复默认 1 小时，超大值截到 30 天
	s.store.SetSessionTTL(0)
	if got := s.store.SessionTTL(); got != time.Hour {
		t.Fatalf("0 应恢复默认 1h，got %v", got)
	}
	s.store.SetSessionTTL(1 << 40)
	if got := s.store.SessionTTL(); got != maxSessionTTL {
		t.Fatalf("超大 TTL 应截到 30 天，got %v", got)
	}
}

// 主动结束会话：同一 ID 下不同协议/模型的所有绑定一并删除，其余会话不受影响。
func TestEndSession(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "a", nil)
	addProviderModel(t, s, up.URL, "b", nil)

	// conv-1 产生两条绑定（不同请求模型名），conv-1x 与 conv-2 各一条
	for _, x := range []struct{ sid, model string }{
		{"conv-1", "a"}, {"conv-1", "b"}, {"conv-1x", "a"}, {"conv-2", "a"},
	} {
		postJSONWithHeader(t, app, "POST", "/v1/chat/completions",
			fmt.Sprintf(`{"model":%q}`, x.model), "X-Session-ID", x.sid)
	}
	if got := len(s.activeSessions()); got != 4 {
		t.Fatalf("应有 4 条绑定，got %d", got)
	}

	req, _ := http.NewRequest("DELETE", app.URL+"/api/sessions?id=conv-1", nil)
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("结束会话应 200, got %d", resp.StatusCode)
	}
	left := s.activeSessions()
	if len(left) != 2 {
		t.Fatalf("应只删除 conv-1 的 2 条绑定: %#v", left)
	}
	for _, x := range left {
		if x.ID != "conv-1x" && x.ID != "conv-2" {
			t.Fatalf("误删了其他会话: %#v", left)
		}
	}

	// 重复结束 / 不存在的会话返回 404；缺参数返回 400
	req, _ = http.NewRequest("DELETE", app.URL+"/api/sessions?id=conv-1", nil)
	resp, err = app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("已结束的会话应 404, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("DELETE", app.URL+"/api/sessions", nil)
	resp, err = app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("缺 id 应 400, got %d", resp.StatusCode)
	}
}

// 设置接口保存会话 TTL，并返回生效值（0 归一为默认 3600 秒）。
func TestSessionTTLSettings(t *testing.T) {
	_, app := newTestServer(t)
	for _, tc := range []struct {
		body string
		want int64
	}{
		{`{"session_ttl_seconds":120}`, 120},
		{`{"session_ttl_seconds":0}`, 3600},
		{`{"session_ttl_seconds":-5}`, 3600},
	} {
		resp, body := postJSONWithHeader(t, app, "PUT", "/api/settings", tc.body, "Content-Type", "application/json")
		if resp.StatusCode != 200 {
			t.Fatalf("settings 应 200: %d %s", resp.StatusCode, body)
		}
		var v struct {
			SessionTTLSeconds int64 `json:"session_ttl_seconds"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		if v.SessionTTLSeconds != tc.want {
			t.Fatalf("%s -> 期望 %d 秒, got %d（%s）", tc.body, tc.want, v.SessionTTLSeconds, body)
		}
	}
}

// 修改 TTL 立即作用于现有绑定：从 1 小时缩短到 1 秒后，旧绑定按最近活跃时间重算。
func TestSessionTTLChangeAppliesImmediately(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "glm", nil)
	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm"}`, "X-Session-ID", "shrink")

	if resp, body := postJSONWithHeader(t, app, "PUT", "/api/settings",
		`{"session_ttl_seconds":1}`, "Content-Type", "application/json"); resp.StatusCode != 200 {
		t.Fatalf("settings 应 200: %d %s", resp.StatusCode, body)
	}
	s.sessMu.Lock()
	var sess session
	for _, v := range s.sessions {
		sess = v
	}
	s.sessMu.Unlock()
	if d := time.Until(sess.expires); d > 2*time.Second {
		t.Fatalf("现有绑定应立即缩短到约 1 秒, 剩余 %v", d)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := len(s.activeSessions()); got != 0 {
		t.Fatalf("缩短 TTL 后旧绑定应过期, got %d", got)
	}
}
