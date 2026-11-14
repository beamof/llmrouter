package app

// 活跃会话列表（GET /api/sessions）：绑定、计数、auto 标记、排序与过期剔除。

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/session"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"P/glm"}`, "X-Session-ID", sid)
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
	if a.ModelID != modelID || a.Requested != "P/glm" || a.Count != 2 {
		t.Fatalf("conv-a 字段不对: %#v", a)
	}
	if a.FirstAt.IsZero() || a.LastAt.IsZero() || !a.Expires.After(a.LastAt) {
		t.Fatalf("时刻字段不对: %#v", a)
	}

	// 过期会话不出现（顺带被清理）
	s.sessionsMgr().Seed("dead", session.Binding{ModelID: "x", Expires: time.Now().Add(-time.Second)})
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
	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm-p/glm"}`, "X-Session-ID", "ttl-1")

	list := s.sessionsMgr().List()
	if len(list) != 1 {
		t.Fatalf("应有 1 条绑定: %#v", list)
	}
	if got := list[0].Expires.Sub(list[0].LastAt); got != time.Second {
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

// 会话维度吞吐：流式请求结束后，活跃会话应带上输出 tokens、输出速度与首字节耗时；
// 同一会话再来一次请求时累计（速度取累计输出 token / 累计生成耗时）。
func TestSessionTokenSpeed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		time.Sleep(20 * time.Millisecond) // 保证首字节耗时可以被测到（毫秒级）
		for i := 0; i < 3; i++ {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":20}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "glm", nil)

	fetch := func() sessionInfo {
		t.Helper()
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
		if len(v.Sessions) != 1 {
			t.Fatalf("应有 1 个活跃会话: %#v", v.Sessions)
		}
		return v.Sessions[0]
	}

	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm-p/glm","stream":true}`, "X-Session-ID", "speed-1")
	got := fetch()
	if got.OutTokens != 20 || got.Count != 1 {
		t.Fatalf("首次请求后应累计 20 输出 tokens / 1 次请求: %#v", got)
	}
	if got.TokPerSec <= 0 || got.LastTokPerSec <= 0 {
		t.Fatalf("应算出输出速度: %#v", got)
	}
	if got.TTFTMs <= 0 || got.LastTTFTMs <= 0 {
		t.Fatalf("流式请求应测到首字节耗时: %#v", got)
	}

	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm-p/glm","stream":true}`, "X-Session-ID", "speed-1")
	got = fetch()
	if got.OutTokens != 40 || got.Count != 2 {
		t.Fatalf("再次请求后应累计 40 输出 tokens / 2 次请求: %#v", got)
	}
	if got.TokPerSec <= 0 || got.LastTokPerSec <= 0 {
		t.Fatalf("累计后仍应有速度: %#v", got)
	}

	// 同时写进了按会话的落盘统计（不含在活跃列表里，但用量页「按会话」可查）
	s.store.Lock()
	stats := append([]*SessionStat(nil), s.store.SessionUsage...)
	s.store.Unlock()
	if len(stats) != 1 {
		t.Fatalf("应有 1 行会话用量: %#v", stats)
	}
	st := stats[0]
	if st.SessionID != "speed-1" || st.Requests != 2 || st.PromptTokens != 10 || st.CompletionTokens != 40 {
		t.Fatalf("会话用量累计不对: %#v", st)
	}
	if st.GenNanos <= 0 || st.TTFTSamples != 2 {
		t.Fatalf("会话用量应带生成耗时与首字节样本: %#v", st)
	}
}

// 会话过期或被主动结束后，按会话的用量仍保留在状态快照里，且全局用量照常记账。
func TestSessionUsageKeptAfterEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":7}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "glm", nil)

	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm-p/glm","stream":true}`, "X-Session-ID", "hist-1")
	req, _ := http.NewRequest("DELETE", app.URL+"/api/sessions?id=hist-1", nil)
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := s.activeSessions(); len(got) != 0 {
		t.Fatalf("会话结束后不应再有活跃绑定: %#v", got)
	}

	st, err := app.Client().Get(app.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Body.Close()
	var v struct {
		Usage        []*UsageStat   `json:"usage"`
		SessionUsage []*SessionStat `json:"session_usage"`
	}
	if err := json.NewDecoder(st.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.SessionUsage) != 1 || v.SessionUsage[0].SessionID != "hist-1" {
		t.Fatalf("结束的会话用量应保留: %#v", v.SessionUsage)
	}
	if len(v.Usage) != 1 {
		t.Fatalf("全局用量也应照常记录: %#v", v.Usage)
	}
}

// 按会话用量随状态落盘：重启（重新 LoadStore）后仍在；删除 Provider 时一并清理。
func TestSessionUsagePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertProvider(&Provider{ID: "p1", Name: "P", BaseURL: "http://example.com"})
	m := &Model{Name: "glm", ProviderID: "p1"}
	st.AddCallUsage("s1", "auto", m, "k1", usageSample{
		Usage:  Usage{Prompt: 10, Cached: 4, Completion: 20},
		Stream: true, TTFT: 50 * time.Millisecond, Gen: time.Second,
	})
	st.AddCallUsage("s1", "auto", m, "k1", usageSample{
		Usage:  Usage{Prompt: 5, Completion: 5},
		Stream: true, TTFT: 30 * time.Millisecond, Gen: 250 * time.Millisecond,
	})

	st2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.SessionUsage) != 1 {
		t.Fatalf("重新加载后应保留 1 行会话用量: %#v", st2.SessionUsage)
	}
	got := st2.SessionUsage[0]
	if got.Requests != 2 || got.PromptTokens != 15 || got.CachedTokens != 4 || got.CompletionTokens != 25 {
		t.Fatalf("会话用量累计不对: %#v", got)
	}
	if got.GenNanos != int64(1250*time.Millisecond) {
		t.Fatalf("生成耗时累计不对: %v", time.Duration(got.GenNanos))
	}
	if got.TTFTSamples != 2 || got.TTFTNanos != int64(80*time.Millisecond) {
		t.Fatalf("首字节累计不对: %#v", got)
	}
	if got.FirstAt.IsZero() || got.LastAt.Before(got.FirstAt) {
		t.Fatalf("时刻字段不对: %#v", got)
	}

	st2.DeleteProvider("p1")
	st3, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st3.SessionUsage) != 0 {
		t.Fatalf("删除 Provider 时应清理其会话用量: %#v", st3.SessionUsage)
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
		{"conv-1", "a-p/a"}, {"conv-1", "b-p/b"}, {"conv-1x", "a-p/a"}, {"conv-2", "a-p/a"},
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

// 未带 X-Session-ID 时按客户端 IP 算 UUID v3 派生会话 ID：同一 IP 内粘同一绑定
// （无视规则变化与源端口），不同 IP 即不同会话，显式头仍然优先。
func TestDerivedSessionFromRemoteAddr(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":"A"}`)
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":"B"}`)
	}))
	defer upA.Close()
	defer upB.Close()

	s, _ := newTestServer(t)
	pA := &Provider{Name: "A", BaseURL: upA.URL}
	pB := &Provider{Name: "B", BaseURL: upB.URL}
	s.store.UpsertProvider(pA)
	s.store.UpsertProvider(pB)
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pA.ID, Priority: 9})
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pB.ID, Priority: 1})
	modelA := s.store.Models[0].ID

	// 直接投递 routes，便于指定 RemoteAddr（httptest 客户端无法控制源端口）
	h := s.routes()
	call := func(addr, sid string) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
		req.RemoteAddr = addr
		if sid != "" {
			req.Header.Set("X-Session-ID", sid)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s 请求失败: %d %s", addr, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	const addr = "10.7.0.5:41234"
	wantID := uuidForIP("10.7.0.5")

	// 1) 首请求按规则选 A，并以派生 ID 绑定
	if got := call(addr, ""); got != `{"ok":"A"}` {
		t.Fatalf("首请求应走 A: %s", got)
	}
	list := s.activeSessions()
	if len(list) != 1 || list[0].ID != wantID || list[0].ModelID != modelA || list[0].Requested != "auto" {
		t.Fatalf("应以 UUIDv3(10.7.0.5) 建立会话: %#v", list)
	}

	// 2) A 被静音：同 IP 仍走 A（绑定优先，源端口变化不影响），换个 IP 则按规则走 B
	mA := s.store.Models[0]
	now := time.Now()
	mA.Mutes = []MuteWindow{{From: now.Add(-time.Minute).Format("15:04"), To: now.Add(time.Minute).Format("15:04")}}
	s.store.UpsertModel(mA)
	if got := call(addr, ""); got != `{"ok":"A"}` {
		t.Fatalf("同源地址应粘在绑定 A: %s", got)
	}
	if got := call("10.7.0.5:41235", ""); got != `{"ok":"A"}` {
		t.Fatalf("同 IP 换源端口应仍粘在绑定 A: %s", got)
	}
	if got := call("10.7.0.6:41234", ""); got != `{"ok":"B"}` {
		t.Fatalf("不同 IP 应按规则走 B: %s", got)
	}

	// 3) 显式 X-Session-ID 优先，不再派生
	if got := call("10.7.0.9:41236", "explicit-1"); got != `{"ok":"B"}` {
		t.Fatalf("显式会话应按规则走 B: %s", got)
	}
	printed := map[string]bool{}
	for _, x := range s.activeSessions() {
		printed[x.ID] = true
	}
	if !printed["explicit-1"] || printed[uuidForIP("10.7.0.9")] {
		t.Fatalf("显式头应覆盖派生 ID: %#v", printed)
	}
}

// uuidForIP 派生会话 ID 的期望值：UUID v3（md5(IP)），与 derivedSessionID 同算法。
func uuidForIP(ip string) string {
	sum := md5.Sum([]byte(ip))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	s := hex.EncodeToString(sum[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// 派生会话 ID 的取值：UUID v3（md5(IP)），同 IP 不论源端口都相同，不同 IP 不同，
// IPv6 只取方括号内的 IP 部分，RemoteAddr 为空则不建会话。
func TestDerivedSessionID(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.RemoteAddr = "10.7.0.5:41234"
	if got, want := derivedSessionID(req), uuidForIP("10.7.0.5"); got != want {
		t.Fatalf("派生 ID 应为 UUIDv3(IP) = %s, got %s", want, got)
	}
	// 同 IP、不同源端口：同一固定 UUID
	req.RemoteAddr = "10.7.0.5:41235"
	if got := derivedSessionID(req); got != uuidForIP("10.7.0.5") {
		t.Fatalf("同 IP 换源端口应得到同一会话 ID, got %q", got)
	}
	// 不同 IP：不同 UUID
	req.RemoteAddr = "10.7.0.6:41234"
	if got := derivedSessionID(req); got == uuidForIP("10.7.0.5") {
		t.Fatal("不同 IP 应得到不同会话 ID")
	}
	// IPv6：只取方括号内的 IP 部分
	req.RemoteAddr = "[2409:8a55:33d0:b100:1202:b5ff:fe86:54]:41234"
	if got, want := derivedSessionID(req), uuidForIP("2409:8a55:33d0:b100:1202:b5ff:fe86:54"); got != want {
		t.Fatalf("IPv6 派生 ID 应为 UUIDv3(IP) = %s, got %s", want, got)
	}
	// UUID 格式：36 字符，8-4-4-4-12，版本位为 3，变体位为 8/9/a/b
	id := uuidForIP("10.7.0.5")
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '3' || id[18] != '-' || id[23] != '-' ||
		(id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b') {
		t.Fatalf("派生 ID 应为 UUID 格式（版本 3）, got %q", id)
	}
	req.RemoteAddr = ""
	if got := derivedSessionID(req); got != "" {
		t.Fatalf("RemoteAddr 为空应不派生会话, got %q", got)
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
	postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"glm-p/glm"}`, "X-Session-ID", "shrink")

	if resp, body := postJSONWithHeader(t, app, "PUT", "/api/settings",
		`{"session_ttl_seconds":1}`, "Content-Type", "application/json"); resp.StatusCode != 200 {
		t.Fatalf("settings 应 200: %d %s", resp.StatusCode, body)
	}
	list := s.sessionsMgr().List()
	if len(list) != 1 {
		t.Fatalf("应有 1 条绑定: %#v", list)
	}
	if d := time.Until(list[0].Expires); d > 2*time.Second {
		t.Fatalf("现有绑定应立即缩短到约 1 秒, 剩余 %v", d)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := len(s.activeSessions()); got != 0 {
		t.Fatalf("缩短 TTL 后旧绑定应过期, got %d", got)
	}
}

// 思考等级：客户端声明值与实际出站值都落进活跃会话与历史会话用量。
// 标准上游字节透传，两个值相同。
func TestSessionsRecordEffortLevel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	p := &Provider{Name: "P", BaseURL: up.URL}
	s.store.UpsertProvider(p)
	s.store.UpsertModel(&Model{Name: "glm", ProviderID: p.ID})

	postJSONWithHeader(t, app, "POST", "/v1/chat/completions",
		`{"model":"P/glm","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`,
		"X-Session-ID", "eff-1")

	// 活跃会话
	resp, err := app.Client().Get(app.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	if b, _ := io.ReadAll(resp.Body); json.Unmarshal(b, &list) != nil || len(list.Sessions) != 1 {
		t.Fatalf("活跃会话列表不对: %s", b)
	}
	if got := list.Sessions[0]; got.EffortDeclared != "medium" || got.EffortSent != "medium" {
		t.Fatalf("活跃会话思考等级 = 声明 %q / 实发 %q, want medium / medium", got.EffortDeclared, got.EffortSent)
	}

	// 历史会话用量（/api/state 里的 session_usage）
	resp2, err := app.Client().Get(app.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var st struct {
		SessionUsage []SessionStat `json:"session_usage"`
	}
	if b, _ := io.ReadAll(resp2.Body); json.Unmarshal(b, &st) != nil || len(st.SessionUsage) != 1 {
		t.Fatalf("session_usage 不对: %s", b)
	}
	if row := st.SessionUsage[0]; row.EffortDeclared != "medium" || row.EffortSent != "medium" {
		t.Fatalf("历史用量思考等级 = 声明 %q / 实发 %q, want medium / medium", row.EffortDeclared, row.EffortSent)
	}
}
