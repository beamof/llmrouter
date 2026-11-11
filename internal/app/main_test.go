package app

import (
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/session"
	"llmrouter/internal/wire"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, client: &http.Client{}}
	app := httptest.NewServer(s.routes())
	t.Cleanup(app.Close)
	return s, app
}

func addProviderModel(t *testing.T, s *Server, baseURL, name string, mut func(*Model)) *Model {
	t.Helper()
	p := &Provider{Name: name + "-p", BaseURL: baseURL, APIKey: "sk-up"}
	s.store.UpsertProvider(p)
	m := &Model{Name: name, ProviderID: p.ID, Priority: 10}
	if mut != nil {
		mut(m)
	}
	s.store.UpsertModel(m)
	return m
}

func postJSON(t *testing.T, app *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	resp, err := app.Client().Post(app.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestPassthroughChat(t *testing.T) {
	respBody := `{"id":"c1","object":"chat.completion","model":"m1","choices":[{"message":{"role":"assistant","content":"你好"}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`
	var gotBody, gotAuth, gotTrace string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotTrace = string(b), r.Header.Get("Authorization"), r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respBody)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m1", nil)

	reqBody := `{"model":"m1","messages":[{"role":"user","content":"hi 你好"}],"temperature":0.7,"stream":false}`
	req, _ := http.NewRequest("POST", app.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-Trace", "t123")
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if string(b) != respBody {
		t.Errorf("响应被改动:\n got: %s\nwant: %s", b, respBody)
	}
	if gotBody != reqBody {
		t.Errorf("请求体被改动:\n got: %s\nwant: %s", gotBody, reqBody)
	}
	if gotAuth != "Bearer sk-up" {
		t.Errorf("上游 Authorization = %q, 应为 provider 密钥", gotAuth)
	}
	if gotTrace != "t123" {
		t.Errorf("自定义请求头未透传: %q", gotTrace)
	}

	var v struct {
		Usage []UsageStat `json:"usage"`
	}
	if err := json.Unmarshal(s.store.SnapshotJSON(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Usage) != 1 || v.Usage[0].Requests != 1 || v.Usage[0].PromptTokens != 3 || v.Usage[0].CompletionTokens != 5 {
		t.Errorf("用量记录不正确: %+v", v.Usage)
	}
}

func TestFailover(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"A is down"}`)
	}))
	up404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"message":"no such route"}}`)
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":"from-B"}`)
	}))
	defer upA.Close()
	defer up404.Close()
	defer upB.Close()

	s, app := newTestServer(t)
	pA := &Provider{Name: "A", BaseURL: upA.URL}
	pN := &Provider{Name: "N", BaseURL: up404.URL}
	pB := &Provider{Name: "B", BaseURL: upB.URL}
	s.store.UpsertProvider(pA)
	s.store.UpsertProvider(pN)
	s.store.UpsertProvider(pB)
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pA.ID, Priority: 9}) // A 优先
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pN.ID, Priority: 5})
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pB.ID, Priority: 1})

	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	if resp.StatusCode != 200 || body != `{"ok":"from-B"}` {
		t.Fatalf("故障转移失败: %d %s", resp.StatusCode, body)
	}
}

func TestFailoverAllFailed(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"max tokens too small"}}`)
	}))
	defer upA.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, upA.URL, "m", nil)
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`)
	if resp.StatusCode != 502 {
		t.Fatalf("全部失败应 502: %d", resp.StatusCode)
	}
	// 502 信息里应带上上游 400 与其错误内容
	if !strings.Contains(body, "HTTP 400") || !strings.Contains(body, "max tokens too small") {
		t.Errorf("失败信息应包含上游错误详情: %s", body)
	}
}

func TestDailyLimit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5}}`) // 每次调用 +10 tokens
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", func(m *Model) { m.DailyLimit = 10 })

	// 未带会话头的请求会按客户端 IP + 端口派生会话并粘住绑定（绑定不套用上限），
	// 这里用不同会话 ID 让每次请求都重新按规则评估。
	if resp, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "X-Session-ID", "limit-1"); resp.StatusCode != 200 {
		t.Fatalf("第一次应成功（10 tokens 未超限）, got %d", resp.StatusCode)
	}
	resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "X-Session-ID", "limit-2")
	if resp.StatusCode != 404 || !strings.Contains(body, "每日上限") {
		t.Fatalf("第二次应超 token 上限: %d %s", resp.StatusCode, body)
	}
	// 上限不影响其他模型
	addProviderModel(t, s, up.URL, "other", nil)
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"other"}`); resp.StatusCode != 200 {
		t.Fatalf("其他模型不应受影响, got %d", resp.StatusCode)
	}
}

func TestMuteTimeWindow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{}") }))
	defer up.Close()

	now := time.Now()
	s, app := newTestServer(t)
	m := addProviderModel(t, s, up.URL, "m", func(m *Model) {
		m.Mutes = []MuteWindow{
			{From: now.Add(-time.Minute).Format("15:04"), To: now.Add(time.Minute).Format("15:04")},
		}
	})
	if resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 404 || !strings.Contains(body, "静音") {
		t.Fatalf("静音时段内应被拒绝: %d %s", resp.StatusCode, body)
	}
	m.Mutes = nil
	s.store.UpsertModel(m)
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatalf("解除静音后应成功, got %d", resp.StatusCode)
	}
}

func TestMutedNowOvernight(t *testing.T) {
	w := []MuteWindow{{From: "22:00", To: "06:00"}}
	at := func(h, mi int) time.Time { return time.Date(2026, 1, 1, h, mi, 0, 0, time.Local) }
	if !mutedNow(w, at(23, 30)) || !mutedNow(w, at(5, 59)) || !mutedNow(w, at(0, 0)) {
		t.Error("跨零点时段内应静音")
	}
	if mutedNow(w, at(6, 0)) || mutedNow(w, at(12, 0)) || mutedNow(w, at(21, 59)) {
		t.Error("时段外不应静音")
	}
}

func TestResponsesPathAndTrailingSlash(t *testing.T) {
	var gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		fmt.Fprint(w, `{"id":"resp_1","output":[]}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL+"/", "r1", nil) // 故意带尾斜杠

	req, _ := http.NewRequest("POST", app.URL+"/v1/responses?beta=true", strings.NewReader(`{"model":"r1","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "resp_1") {
		t.Fatalf("responses 代理失败: %d %s", resp.StatusCode, b)
	}
	if gotPath != "/responses" || gotQuery != "beta=true" {
		t.Errorf("上游收到 path=%q query=%q, 想要 /responses?beta=true", gotPath, gotQuery)
	}
}

func TestStreamPassthrough(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)

	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m","stream":true}`)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("流式响应异常: %d %s", resp.StatusCode, body)
	}
	if body != sse {
		t.Errorf("SSE 内容被改动:\n got: %q\nwant: %q", body, sse)
	}
}

func TestModelsList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "zzz", nil)
	addProviderModel(t, s, up.URL, "zzz", nil) // 同名去重
	mA := addProviderModel(t, s, up.URL, "aaa", nil)

	if resp, _ := postJSON(t, app, "/v1/models", ""); resp.StatusCode != 405 { // POST 应 405，换 GET
		t.Fatalf("POST /v1/models 应 405, got %d", resp.StatusCode)
	}
	ids := func() []string {
		t.Helper()
		r, err := app.Client().Get(app.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var v struct {
			Data []struct {
				ID string
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(v.Data))
		for i, d := range v.Data {
			out[i] = d.ID
		}
		return out
	}

	// 默认只有 auto 虚拟模型对外暴露，真实模型需逐条打开开关
	if got := ids(); len(got) != 1 || got[0] != "auto" {
		t.Errorf("默认应只暴露 auto，got %v", got)
	}

	// 打开 aaa 的暴露开关后，auto 之后出现 aaa（去重排序）
	if resp, body := postJSON(t, app, "/api/models/"+mA.ID+"/expose", `{"expose":true}`); resp.StatusCode != 200 {
		t.Fatalf("设置 expose 失败: %d %s", resp.StatusCode, body)
	}
	if got := ids(); len(got) != 2 || got[0] != "auto" || got[1] != "aaa" {
		t.Errorf("开启暴露后模型列表不正确: %v", got)
	}

	// 关闭后重新隐藏
	if resp, body := postJSON(t, app, "/api/models/"+mA.ID+"/expose", `{"expose":false}`); resp.StatusCode != 200 {
		t.Fatalf("关闭 expose 失败: %d %s", resp.StatusCode, body)
	}
	if got := ids(); len(got) != 1 || got[0] != "auto" {
		t.Errorf("关闭暴露后应只剩 auto，got %v", got)
	}
}

// /v1/models 的上下文声明：真实模型名用自身值，路由组名取成员最小值，
// 未声明（0）的条目不带这两个字段。
func TestModelsListContextLength(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()

	s, app := newTestServer(t)
	mBig := addProviderModel(t, s, up.URL, "big", func(m *Model) { m.ContextLength = 200000 })
	addProviderModel(t, s, up.URL, "small", func(m *Model) { m.ContextLength = 64000 })
	mUnknown := addProviderModel(t, s, up.URL, "unknown", nil) // 未声明上下文

	items := func() map[string]map[string]any {
		t.Helper()
		r, err := app.Client().Get(app.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var v struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, d := range v.Data {
			out[d["id"].(string)] = d
		}
		return out
	}
	num := func(m map[string]any, k string) (int64, bool) {
		f, ok := m[k].(float64)
		return int64(f), ok
	}

	// 组名 auto：成员 big/small/unknown → 取已声明的最小值 64000
	got := items()["auto"]
	for _, k := range []string{"context_length", "max_model_len"} {
		if n, ok := num(got, k); !ok || n != 64000 {
			t.Errorf("auto.%s = %v（ok=%v），want 64000", k, got[k], ok)
		}
	}

	// 路由组的「上下文大小」手动配置优先于成员最小值，清空后回到成员最小值
	g := s.store.GroupsList()[0]
	setGroupCtx := func(n int64) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"note":%q,"context_length":%d}`, g.Name, g.Note, n)
		req, _ := http.NewRequest(http.MethodPut, app.URL+"/api/groups/"+g.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("更新路由组失败: %d", resp.StatusCode)
		}
	}
	setGroupCtx(32000)
	if n, _ := num(items()["auto"], "context_length"); n != 32000 {
		t.Errorf("组配置应覆盖成员最小值，got %d", n)
	}
	setGroupCtx(0)
	if n, _ := num(items()["auto"], "context_length"); n != 64000 {
		t.Errorf("清空组配置后应回落到成员最小值 64000，got %d", n)
	}

	// 暴露真实模型名：各用自己的值；未声明的不输出这两个字段
	if resp, body := postJSON(t, app, "/api/models/"+mBig.ID+"/expose", `{"expose":true}`); resp.StatusCode != 200 {
		t.Fatalf("设置 expose 失败: %d %s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, app, "/api/models/"+mUnknown.ID+"/expose", `{"expose":true}`); resp.StatusCode != 200 {
		t.Fatalf("设置 expose 失败: %d %s", resp.StatusCode, body)
	}
	all := items()
	if n, ok := num(all["big"], "context_length"); !ok || n != 200000 {
		t.Errorf("big.context_length = %v，want 200000", all["big"]["context_length"])
	}
	if n, ok := num(all["big"], "max_model_len"); !ok || n != 200000 {
		t.Errorf("big.max_model_len = %v，want 200000", all["big"]["max_model_len"])
	}
	for _, k := range []string{"context_length", "max_model_len"} {
		if v, ok := all["unknown"][k]; ok {
			t.Errorf("未声明的模型不应输出 %s，got %v", k, v)
		}
	}
}

func TestAutoModel(t *testing.T) {
	var gotA, gotB string
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotA = string(b)
		fmt.Fprint(w, `{"ok":"A"}`)
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotB = string(b)
		fmt.Fprint(w, `{"ok":"B"}`)
	}))
	defer upA.Close()
	defer upB.Close()

	s, app := newTestServer(t)
	pA := &Provider{Name: "A", BaseURL: upA.URL}
	pB := &Provider{Name: "B", BaseURL: upB.URL}
	s.store.UpsertProvider(pA)
	s.store.UpsertProvider(pB)
	mA := &Model{Name: "model-a", ProviderID: pA.ID, Priority: 9}
	s.store.UpsertModel(mA)
	s.store.UpsertModel(&Model{Name: "model-b", ProviderID: pB.ID, Priority: 1})

	// auto → 选优先级最高的 A；上游收到的 model 已替换为 model-a，其余字段原样
	// 未带会话头的请求会自动派生会话并粘住绑定，这里每次用不同会话 ID 让规则重新生效。
	reqBody := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	if _, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", reqBody, "X-Session-ID", "auto-1"); body != `{"ok":"A"}` {
		t.Fatalf("auto 应选 A: %s", body)
	}
	if !strings.Contains(gotA, `"model":"model-a"`) {
		t.Errorf("上游收到的 model 应替换为 model-a: %s", gotA)
	}
	if !strings.Contains(gotA, `"hi"`) || !strings.Contains(gotA, `"temperature":0.5`) {
		t.Errorf("其余字段应原样保留: %s", gotA)
	}

	// A 静音 → auto 自动落到 B
	now := time.Now()
	mA.Mutes = []MuteWindow{{From: now.Add(-time.Minute).Format("15:04"), To: now.Add(time.Minute).Format("15:04")}}
	s.store.UpsertModel(mA)
	if _, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", reqBody, "X-Session-ID", "auto-2"); body != `{"ok":"B"}` {
		t.Fatalf("A 静音后 auto 应选 B: %s", body)
	}
	if !strings.Contains(gotB, `"model":"model-b"`) {
		t.Errorf("B 收到的 model 应为 model-b: %s", gotB)
	}

	// 改名：auto 失效，新名生效
	respS, bodyS := postJSONWithHeader(t, app, "PUT", "/api/settings", `{"auto_model":"pick"}`, "Content-Type", "application/json")
	if respS.StatusCode != 200 {
		t.Fatalf("改名失败: %d %s", respS.StatusCode, bodyS)
	}
	if resp, _ := postJSON(t, app, "/v1/chat/completions", reqBody); resp.StatusCode != 404 {
		t.Fatalf("改名后 auto 应 404")
	}
	reqBody2 := `{"model":"pick","messages":[]}`
	if _, body := postJSON(t, app, "/v1/chat/completions", reqBody2); body != `{"ok":"B"}` {
		t.Fatalf("新名 pick 应生效: %s", body)
	}

	// withModel 单元：非 JSON 原样返回
	if got := withModel([]byte(`not json`), "x"); string(got) != `not json` {
		t.Errorf("非 JSON 应原样返回: %s", got)
	}
}

func TestReorderPriority(t *testing.T) {
	s, app := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	p := &Provider{Name: "p", BaseURL: up.URL}
	s.store.UpsertProvider(p)
	for i := 0; i < 3; i++ {
		s.store.UpsertModel(&Model{Name: "same", ProviderID: p.ID})
	}
	ms := s.store.Models
	// 重排为 c(第3个) 最优先
	if _, err := http.Post(app.URL+"/api/models/reorder", "application/json",
		strings.NewReader(`{"ids":["`+ms[2].ID+`","`+ms[0].ID+`","`+ms[1].ID+`"]}`)); err != nil {
		t.Fatal(err)
	}
	cands := s.store.Candidates("same", time.Now())
	if len(cands) != 3 || cands[0].Model.ID != ms[2].ID || cands[0].Model.Priority != 3 {
		t.Errorf("优先级排序错误: %+v", cands)
	}
}

func TestAdminAuthAndCRUD(t *testing.T) {
	s, app := newTestServer(t)
	s.adminToken = "secret"

	resp, body := postJSON(t, app, "/api/providers", `{"name":"x","base_url":"https://x.example/v1"}`)
	if resp.StatusCode != 401 {
		t.Fatalf("无令牌应 401: %d %s", resp.StatusCode, body)
	}

	do := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, app.URL+path, strings.NewReader(body))
		req.Header.Set("X-Admin-Token", "secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp, _ = postJSONWithHeader(t, app, "POST", "/api/providers", `{"name":"x","base_url":"https://x.example/v1"}`, "X-Admin-Token", "secret")
	if resp.StatusCode != 200 {
		t.Fatalf("创建 provider 失败: %d", resp.StatusCode)
	}
	pid := s.store.Providers[len(s.store.Providers)-1].ID

	r := do("POST", "/api/models", `{"name":"gpt-x","provider_id":"`+pid+`","priority":1,"daily_limit":5}`)
	if r.StatusCode != 200 {
		t.Fatalf("创建 model 失败: %d", r.StatusCode)
	}
	if len(s.store.Models) != 1 {
		t.Fatalf("model 数量不对: %d", len(s.store.Models))
	}
	r = do("PUT", "/api/models/"+s.store.Models[0].ID, `{"name":"gpt-y","provider_id":"`+pid+`","daily_limit":0}`)
	if r.StatusCode != 200 || s.store.Models[0].Name != "gpt-y" {
		t.Fatalf("更新 model 失败")
	}
	r = do("DELETE", "/api/providers/"+pid, "")
	if r.StatusCode != 200 || len(s.store.Models) != 0 {
		t.Fatalf("删除 provider 应级联删模型")
	}
}

// 连续输错 5 次后锁定：此后即使提交正确令牌也直接拒绝，且不再做校验
func TestAdminTokenLockout(t *testing.T) {
	s, app := newTestServer(t)
	s.adminToken = "secret"

	wrong := func() *http.Response {
		r, _ := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "bad")
		return r
	}
	// 前 4 次仍是普通错误
	for i := 1; i < adminMaxFails; i++ {
		if r := wrong(); r.StatusCode != 401 {
			t.Fatalf("第 %d 次错误应 401, got %d", i, r.StatusCode)
		}
	}
	// 第 5 次触发锁定
	if r := wrong(); r.StatusCode != 401 {
		t.Fatalf("第 %d 次错误应 401, got %d", adminMaxFails, r.StatusCode)
	}
	fails := s.authGuard().Failures()
	if n := len(fails); n != 1 {
		t.Fatalf("应记录 1 个来源的失败, got %d", n)
	}
	var lockedIP string
	for ip, f := range fails {
		lockedIP = ip
		if f.LockedUntil.IsZero() {
			t.Fatal("连续 5 次失败后应处于锁定状态")
		}
	}
	// 锁定期间即使令牌正确也被拒绝
	r, body := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "secret")
	if r.StatusCode != 401 {
		t.Fatalf("锁定期内正确令牌也应 401, got %d %s", r.StatusCode, body)
	}
	if !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("应提示锁定, got %s", body)
	}
	// 锁定到期后恢复校验，正确令牌可进入
	s.authGuard().SetLockedUntil(lockedIP, time.Now().Add(-time.Second))
	if r, _ := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "secret"); r.StatusCode != 200 {
		t.Fatalf("锁定到期后正确令牌应放行, got %d", r.StatusCode)
	}
}

// 令牌正确时清空失败计数，不会因为零散的输错被误锁
func TestAdminTokenFailCountReset(t *testing.T) {
	s, app := newTestServer(t)
	s.adminToken = "secret"

	for i := 0; i < adminMaxFails-1; i++ {
		postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "bad")
	}
	if r, _ := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "secret"); r.StatusCode != 200 {
		t.Fatalf("正确令牌应放行, got %d", r.StatusCode)
	}
	n := len(s.authGuard().Failures())
	if n != 0 {
		t.Fatalf("成功后失败记录应清空, got %d", n)
	}
}

// 未配置管理令牌时后台开放，不受失败计数影响
func TestAdminTokenNoLockoutWhenDisabled(t *testing.T) {
	_, app := newTestServer(t)
	for i := 0; i < adminMaxFails+3; i++ {
		if r, _ := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "bad"); r.StatusCode != 200 {
			t.Fatalf("未启用鉴权时应始终 200, got %d", r.StatusCode)
		}
	}
}

// 默认不采信转发头（防伪造绕过），开启 trustProxy 后才使用
func TestClientIPTrustProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	s := &Server{}
	if got := s.clientIP(req); got != "10.0.0.9" {
		t.Fatalf("默认应取 TCP 对端, got %q", got)
	}
	s.trustProxy = true
	if got := s.clientIP(req); got != "1.2.3.4" {
		t.Fatalf("开启后应取 XFF 首个, got %q", got)
	}
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "9.9.9.9")
	if got := s.clientIP(req); got != "9.9.9.9" {
		t.Fatalf("应回退到 X-Real-IP, got %q", got)
	}
}

func postJSONWithHeader(t *testing.T, app *httptest.Server, method, path, body, hk, hv string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, app.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hk, hv)
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestFindBalances(t *testing.T) {
	raw := `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.25","granted_balance":"10.25"}],"data":{"total_credit":88.5}}`
	got := findBalances([]byte(raw))
	joined := strings.Join(got, "; ")
	for _, want := range []string{"balance_infos.0.total_balance = 110.25", "balance_infos.0.granted_balance = 10.25", "data.total_credit = 88.5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("未识别 %s, got: %v", want, got)
		}
	}
	// 智谱风格：balanceType 这类非数值字段不应出现，totalBalance 排在最前
	zhipu := `{"data":{"isAvailable":true,"balanceInfo":[{"balanceType":"devResource","totalBalance":"66.60","grantedBalance":"0.00"}]}}`
	got2 := findBalances([]byte(zhipu))
	if len(got2) < 2 {
		t.Fatalf("智谱余额识别失败: %v", got2)
	}
	if !strings.HasPrefix(got2[0], "data.balanceInfo.0.totalBalance") {
		t.Errorf("totalBalance 应排最前: %v", got2)
	}
	for _, g := range got2 {
		if strings.Contains(g, "balanceType") {
			t.Errorf("非数值字段不应被当作余额: %v", got2)
		}
	}
	if got := findBalances([]byte(`not json`)); got != nil {
		t.Errorf("非 JSON 应返回 nil, got %v", got)
	}
}

func TestBalanceEndpoint(t *testing.T) {
	cases := []struct {
		base, balURL, want string
	}{
		{"https://open.bigmodel.cn/api/paas/v4", "", "https://open.bigmodel.cn/api/monitor/usage/quota/limit"},
		{"https://open.bigmodel.cn/api/coding/paas/v4/", "", "https://open.bigmodel.cn/api/monitor/usage/quota/limit"},
		{"https://api.deepseek.com/v1", "", "https://api.deepseek.com/user/balance"},
		{"https://api.deepseek.com/v1", "https://x.example/balance", "https://x.example/balance"},
		{"https://api.example.com/v1", "", ""},
	}
	for _, c := range cases {
		if got := balanceEndpoint(Provider{BaseURL: c.base, BalanceURL: c.balURL}); got != c.want {
			t.Errorf("balanceEndpoint(%q, %q) = %q, want %q", c.base, c.balURL, got, c.want)
		}
	}
}

func TestZhipuQuotaSummary(t *testing.T) {
	// 智谱 Coding Plan 真实响应结构
	raw := `{"code":200,"msg":"操作成功","data":{"limits":[` +
		`{"type":"TIME_LIMIT","usage":100,"currentValue":1,"remaining":99,"percentage":1,"nextResetTime":1796042512983},` +
		`{"type":"TOKENS_LIMIT","percentage":32,"nextResetTime":1793982631361}],"level":"lite"},"success":true}`
	got := zhipuQuotaSummary([]byte(raw))
	if len(got) != 3 {
		t.Fatalf("应产出 3 条文案: %v", got)
	}
	if !strings.Contains(got[0], "MCP 次数 1/100（剩 99）") || !strings.Contains(got[0], "重置") {
		t.Errorf("MCP 文案不对: %v", got[0])
	}
	if !strings.Contains(got[1], "Token 额度已用 32%") {
		t.Errorf("Token 额度文案不对: %v", got[1])
	}
	if got[2] != "套餐等级 lite" {
		t.Errorf("套餐等级文案不对: %v", got[2])
	}
	// 非智谱格式应返回 nil
	if zhipuQuotaSummary([]byte(`{"balance_infos":[]}`)) != nil {
		t.Error("非智谱格式应返回 nil")
	}
}

func TestStateEmptyArrays(t *testing.T) {
	_, app := newTestServer(t)
	resp, err := app.Client().Get(app.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	for _, want := range []string{`"providers":[]`, `"models":[]`, `"usage":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("空状态应输出 %s，实际: %s", want, body)
		}
	}
}

func TestUpstreamModels(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != "GET" || r.URL.Path != "/models" {
			t.Errorf("上游收到 %s %s, 想要 GET /models", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"a"},{"id":""}]}`)
	}))
	defer up.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer bad.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "x", nil)
	addProviderModel(t, s, bad.URL, "y", nil)

	// 正常：去重 + 排序 + 带上 provider 密钥
	resp, err := app.Client().Get(app.URL + "/api/providers/" + s.store.Providers[0].ID + "/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `{"models":["a","b"]}`+"\n" {
		t.Errorf("上游模型列表 = %s", b)
	}
	if gotAuth != "Bearer sk-up" {
		t.Errorf("上游 Authorization = %q", gotAuth)
	}

	// 上游 404：返回 502 与错误信息
	resp2, err := app.Client().Get(app.URL + "/api/providers/" + s.store.Providers[1].ID + "/models")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 502 || !strings.Contains(string(b2), "HTTP 404") {
		t.Errorf("上游失败应 502: %d %s", resp2.StatusCode, b2)
	}
}

func TestModelTest(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/models" {
			t.Errorf("探测应发 GET /models, 实际 %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"good"},{"id":"other"}]}`)
	}))
	defer up.Close()
	authFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"Invalid API key"}}`)
	}))
	defer authFail.Close()

	s, app := newTestServer(t)
	mOK := addProviderModel(t, s, up.URL, "good", nil)
	mMiss := addProviderModel(t, s, up.URL, "ghost", nil)
	mAuth := addProviderModel(t, s, authFail.URL, "x", nil)
	mDead := addProviderModel(t, s, "http://127.0.0.1:1", "y", nil) // 无人监听

	run := func(id string) map[string]any {
		t.Helper()
		resp, body := postJSON(t, app, "/api/models/"+id+"/test", "")
		if resp.StatusCode != 200 {
			t.Fatalf("测试接口自身应 200: %d %s", resp.StatusCode, body)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	if v := run(mOK.ID); v["level"] != "ok" || !strings.Contains(v["message"].(string), "模型名在上游列表中") {
		t.Errorf("列表内模型探测结果不正确: %v", v)
	}
	if v := run(mMiss.ID); v["level"] != "warn" || !strings.Contains(v["message"].(string), `没有 "ghost"`) {
		t.Errorf("列表外模型应为 warn: %v", v)
	}
	if v := run(mAuth.ID); v["level"] != "warn" || !strings.Contains(v["message"].(string), "鉴权失败") || !strings.Contains(v["message"].(string), "Invalid API key") {
		t.Errorf("401 应提示鉴权失败: %v", v)
	}
	if v := run(mDead.ID); v["level"] != "fail" || !strings.Contains(v["message"].(string), "连接失败") {
		t.Errorf("死地址应为 fail: %v", v)
	}
	if gotAuth != "Bearer sk-up" {
		t.Errorf("探测应携带 provider 密钥, 实际 %q", gotAuth)
	}

	// 探测不计入用量
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	if err := json.Unmarshal(s.store.SnapshotJSON(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Usage) != 0 {
		t.Errorf("探测不应计入用量: %+v", st.Usage)
	}
}

func TestExtractModelIDs(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"openai", `{"data":[{"id":"b"},{"id":"a"},{"id":"a"}]}`, []string{"b", "a"}},
		{"glm-anthropic", `{"models":[{"display_name":"glm-5.3","slug":"glm-5.3"},{"slug":"glm-4.6"}]}`, []string{"glm-5.3", "glm-4.6"}},
		{"字符串数组", `{"models":["x","y"]}`, []string{"x", "y"}},
		{"name 字段", `{"models":[{"name":"z"}]}`, []string{"z"}},
		{"坏 JSON", `not json`, nil},
	}
	for _, c := range cases {
		got := extractModelIDs([]byte(c.in))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestPerProtocolURL(t *testing.T) {
	chatOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			fmt.Fprint(w, `{"ok":"chat"}`)
			return
		}
		http.NotFound(w, r) // 若 responses 误走此基址会 404 暴露错误
	}))
	defer chatOnly.Close()
	respOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			fmt.Fprint(w, `{"ok":"responses"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer respOnly.Close()
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer both.Close()

	s, app := newTestServer(t)
	// 双基址 provider：chat 与 responses 各走各的地址
	dual := &Provider{Name: "dual", BaseURL: chatOnly.URL, ResponsesURL: respOnly.URL}
	s.store.UpsertProvider(dual)
	s.store.UpsertModel(&Model{Name: "m", ProviderID: dual.ID})
	if _, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); body != `{"ok":"chat"}` {
		t.Errorf("chat 请求应走 Chat 基址: %s", body)
	}
	if _, body := postJSON(t, app, "/v1/responses", `{"model":"m"}`); body != `{"ok":"responses"}` {
		t.Errorf("responses 请求应走 Responses 基址: %s", body)
	}

	// 未单独配置 responses 基址时，两种协议都走 BaseURL
	fallback := &Provider{Name: "fallback", BaseURL: both.URL}
	s.store.UpsertProvider(fallback)
	s.store.UpsertModel(&Model{Name: "f", ProviderID: fallback.ID})
	if _, body := postJSON(t, app, "/v1/chat/completions", `{"model":"f"}`); body != `{"path":"/chat/completions"}` {
		t.Errorf("chat 回落错误: %s", body)
	}
	if _, body := postJSON(t, app, "/v1/responses", `{"model":"f"}`); body != `{"path":"/responses"}` {
		t.Errorf("responses 回落错误: %s", body)
	}
}

func TestAPIKeys(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)

	// 未配置任何 Key：开放访问
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatalf("无 Key 时应开放访问, got %d", resp.StatusCode)
	}

	// 生成 Key
	resp, body := postJSON(t, app, "/api/keys", `{"name":"测试客户端"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("生成 Key 失败: %d %s", resp.StatusCode, body)
	}
	var k APIKey
	if err := json.Unmarshal([]byte(body), &k); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "sk-r-") || k.ID == "" {
		t.Fatalf("生成的 Key 不合法: %+v", k)
	}

	// 有 Key 后：无鉴权 401，带 Key 200 且用量记到该 Key
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 401 {
		t.Fatalf("配置 Key 后无鉴权应 401, got %d", resp.StatusCode)
	}
	r1, b1 := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "Authorization", "Bearer "+k.Key)
	if r1.StatusCode != 200 {
		t.Fatalf("带 Key 请求失败: %d %s", r1.StatusCode, b1)
	}
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	if err := json.Unmarshal(s.store.SnapshotJSON(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Usage) != 2 || st.Usage[1].KeyID != k.ID {
		t.Errorf("用量应按 Key 区分（一条免鉴权、一条带 Key）: %+v", st.Usage)
	}

	// 环境变量令牌仍然有效，记为 "env"
	s.apiToken = "env-tok"
	if r, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "Authorization", "Bearer env-tok"); r.StatusCode != 200 {
		t.Fatal("环境变量令牌应可用")
	}

	// 删除 Key 后立即失效
	req, _ := http.NewRequest("DELETE", app.URL+"/api/keys/"+k.ID, nil)
	if resp, err := app.Client().Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("删除 Key 失败: %v", err)
	}
	if r, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "Authorization", "Bearer "+k.Key); r.StatusCode != 401 {
		t.Fatal("删除 Key 后该 Key 应失效")
	}
}

func TestSessionBinding(t *testing.T) {
	aDown := false
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aDown {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"A down"}`)
			return
		}
		fmt.Fprint(w, `{"ok":"A"}`)
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":"B"}`)
	}))
	defer upA.Close()
	defer upB.Close()

	s, app := newTestServer(t)
	pA := &Provider{Name: "A", BaseURL: upA.URL}
	pB := &Provider{Name: "B", BaseURL: upB.URL}
	s.store.UpsertProvider(pA)
	s.store.UpsertProvider(pB)
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pA.ID, Priority: 9})
	s.store.UpsertModel(&Model{Name: "m", ProviderID: pB.ID, Priority: 1})

	sess := func() (*http.Response, string) {
		return postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "X-Session-ID", "conv-1")
	}

	// 1) 会话首请求按规则选 A
	if _, b := sess(); b != `{"ok":"A"}` {
		t.Fatalf("会话首请求应选 A: %s", b)
	}
	// 2) 规则变化（A 被静音）：无会话头走 B，带会话头仍走 A（绑定优先于规则）
	mA := s.store.Models[0]
	now := time.Now()
	mA.Mutes = []MuteWindow{{From: now.Add(-time.Minute).Format("15:04"), To: now.Add(time.Minute).Format("15:04")}}
	s.store.UpsertModel(mA)
	if _, b := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); b != `{"ok":"B"}` {
		t.Fatalf("无会话请求应走 B: %s", b)
	}
	if _, b := sess(); b != `{"ok":"A"}` {
		t.Fatalf("会话绑定应无视静音仍走 A: %s", b)
	}
	// 3) A 故障：会话 fallback 到 B 并重新绑定
	aDown = true
	if _, b := sess(); b != `{"ok":"B"}` {
		t.Fatalf("绑定失败应 fallback 到 B: %s", b)
	}
	// 4) A 恢复后，会话仍走 B（已重绑）
	aDown = false
	if _, b := sess(); b != `{"ok":"B"}` {
		t.Fatalf("重绑后会话应继续走 B: %s", b)
	}
	// 5) 不同会话头互不影响，按规则走 B（A 仍静音）
	if _, b := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "X-Session-ID", "conv-2"); b != `{"ok":"B"}` {
		t.Fatalf("新会话应按规则走 B: %s", b)
	}
}

func TestSessionExpiry(t *testing.T) {
	s := &Server{}
	s.sessionsMgr().Seed("k", session.Binding{ModelID: "m1", Expires: time.Now().Add(-time.Minute)})
	if _, ok := s.getSession("k"); ok {
		t.Error("过期会话应失效")
	}
	s.setSession("k", "m2", "key-1", "glm")
	if id, ok := s.getSession("k"); !ok || id != "m2" {
		t.Error("会话应写入并可读取")
	}
	s.clearSession("k")
	if _, ok := s.getSession("k"); ok {
		t.Error("会话应可清除")
	}
}

func TestStreamUsageCounting(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m","stream":true}`)
	if resp.StatusCode != 200 || body != sse {
		t.Fatalf("SSE 应原样透传: %d %q", resp.StatusCode, body)
	}
	// 记账发生在响应体写完之后，轮询等待 handler 收尾
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	for i := 0; i < 50; i++ {
		if err := json.Unmarshal(s.store.SnapshotJSON(), &st); err != nil {
			t.Fatal(err)
		}
		if len(st.Usage) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(st.Usage) != 1 || st.Usage[0].PromptTokens != 11 || st.Usage[0].CompletionTokens != 22 {
		t.Errorf("流式 usage 未记账: %+v", st.Usage)
	}
}

func TestExtractStreamUsage(t *testing.T) {
	// responses 风格：usage 嵌套在 response 对象里，且含二级嵌套
	respStyle := []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":107,"input_tokens_details":{"text_tokens":107},"output_tokens":36}}}`)
	if u := wire.ExtractStreamUsage(respStyle); u.Prompt != 107 || u.Completion != 36 {
		t.Errorf("responses 风格提取错误: %d/%d", u.Prompt, u.Completion)
	}
	// chat 风格
	chatStyle := []byte(`data: {"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	if u := wire.ExtractStreamUsage(chatStyle); u.Prompt != 5 || u.Completion != 6 {
		t.Errorf("chat 风格提取错误: %d/%d", u.Prompt, u.Completion)
	}
	// 无 usage / usage:null
	if u := wire.ExtractStreamUsage([]byte("data: [DONE]")); u.Prompt != 0 || u.Completion != 0 {
		t.Errorf("无 usage 应为 0: %d/%d", u.Prompt, u.Completion)
	}
	if u := wire.ExtractStreamUsage([]byte(`{"usage":null}`)); u.Prompt != 0 || u.Completion != 0 {
		t.Errorf("usage:null 应为 0: %d/%d", u.Prompt, u.Completion)
	}
}

func TestCachedTokens(t *testing.T) {
	// 流式尾部嗅探应提取缓存命中量
	tail := []byte(`data: {"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}}`)
	if got := wire.ExtractStreamUsage(tail); got.Prompt != 100 || got.Cached != 80 || got.Completion != 10 {
		t.Errorf("流式缓存提取错误: %+v", got)
	}
	// 无缓存字段时 Cached 为 0
	tail2 := []byte(`data: {"usage":{"prompt_tokens":50,"completion_tokens":5}}`)
	if got := wire.ExtractStreamUsage(tail2); got.Prompt != 50 || got.Cached != 0 {
		t.Errorf("无缓存字段应记 0: %+v", got)
	}

	// 非流式响应经代理后应记下 cached_tokens
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900}}}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatal("请求失败")
	}
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	json.Unmarshal(s.store.SnapshotJSON(), &st)
	if len(st.Usage) != 1 || st.Usage[0].CachedTokens != 900 || st.Usage[0].PromptTokens != 1000 {
		t.Errorf("非流式缓存未记账: %+v", st.Usage)
	}
}

func TestNoModelField(t *testing.T) {
	_, app := newTestServer(t)
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"messages":[]}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "model") {
		t.Fatalf("缺 model 字段应 400: %d %s", resp.StatusCode, body)
	}
	resp, body = postJSON(t, app, "/v1/chat/completions", `{"model":"ghost"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("未知模型应 404: %d %s", resp.StatusCode, body)
	}
}

// 上游失败应写入异常日志，含状态码、Provider 与上游错误详情。
// 429 单独归为 muted 类型（会触发自动静音），这里用 500 验证普通失败路径。
func TestLogUpstreamFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"upstream boom"}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	m := addProviderModel(t, s, up.URL, "m", nil)
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 502 {
		t.Fatalf("应 502, got %d", resp.StatusCode)
	}

	s.store.Lock()
	logs := append([]*LogEntry(nil), s.store.Logs...)
	s.store.Unlock()
	if len(logs) != 2 {
		t.Fatalf("应记录 2 条（单上游失败 + 全部失败），got %d", len(logs))
	}
	var up1, all *LogEntry
	for _, l := range logs {
		switch l.Kind {
		case kindUpstream:
			up1 = l
		case kindAllFailed:
			all = l
		}
	}
	if up1 == nil || all == nil {
		t.Fatalf("缺少预期日志: %+v", logs)
	}
	if up1.Status != 500 || up1.ProviderID != m.ProviderID || up1.Model != "m" {
		t.Errorf("上游失败日志字段不对: %+v", up1)
	}
	if !strings.Contains(up1.Detail, "upstream boom") {
		t.Errorf("应含上游错误详情: %q", up1.Detail)
	}
	if all.Status != 502 || !strings.Contains(all.Message, "全部失败") {
		t.Errorf("汇总日志不对: %+v", all)
	}
}

// 没有可用上游、请求体不合法、API Key 无效都要记日志
func TestLogRoutingAndAuthFailures(t *testing.T) {
	s, app := newTestServer(t)

	// 未知模型 → 404 no_candidate
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"ghost"}`); resp.StatusCode != 404 {
		t.Fatalf("应 404, got %d", resp.StatusCode)
	}
	// 缺 model 字段 → 400 bad_request
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"messages":[]}`); resp.StatusCode != 400 {
		t.Fatalf("应 400, got %d", resp.StatusCode)
	}
	// 配置 Key 后用错误 Key → 401 auth
	s.store.AddAPIKey("k")
	if resp, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`, "Authorization", "Bearer bad"); resp.StatusCode != 401 {
		t.Fatalf("应 401, got %d", resp.StatusCode)
	}

	s.store.Lock()
	kinds := map[string]int{}
	for _, l := range s.store.Logs {
		kinds[l.Kind]++
	}
	s.store.Unlock()
	for _, want := range []string{kindNoCandidate, kindBadRequest, kindAuth} {
		if kinds[want] == 0 {
			t.Errorf("缺少 %s 日志，实际: %v", want, kinds)
		}
	}
}

// 日志条数有上限，超出时丢弃最旧的；清空接口能清干净
func TestLogCapAndClear(t *testing.T) {
	s, app := newTestServer(t)
	s.adminToken = "tok"

	for i := 0; i < maxLogs+20; i++ {
		s.logFail(LogEntry{Kind: kindUpstream, Message: fmt.Sprintf("err-%d", i)})
	}
	s.store.Lock()
	n, first := len(s.store.Logs), s.store.Logs[0].Message
	s.store.Unlock()
	if n != maxLogs {
		t.Fatalf("应截断到 %d 条, got %d", maxLogs, n)
	}
	if first != "err-20" {
		t.Fatalf("应丢弃最旧的 20 条, 首条为 %q", first)
	}

	req, _ := http.NewRequest("DELETE", app.URL+"/api/logs", nil)
	req.Header.Set("X-Admin-Token", "tok")
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("清空应 200, got %d", resp.StatusCode)
	}
	s.store.Lock()
	n = len(s.store.Logs)
	s.store.Unlock()
	if n != 0 {
		t.Fatalf("清空后应为 0, got %d", n)
	}
}

// /api/state 必须返回 logs 数组（空时是 [] 而不是 null），前端才不会拿到 null
func TestStateIncludesLogs(t *testing.T) {
	_, app := newTestServer(t)
	resp, body := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "")
	if resp.StatusCode != 200 {
		t.Fatalf("应 200, got %d", resp.StatusCode)
	}
	var v struct {
		Logs *[]LogEntry `json:"logs"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Logs == nil {
		t.Fatal("logs 不应为 null")
	}
	if len(*v.Logs) != 0 {
		t.Fatalf("空状态应为空数组, got %d", len(*v.Logs))
	}
}

// TryAcquire/Release 的计数语义：满额拒绝、释放后可再取、归零即删键
func TestTryAcquireRelease(t *testing.T) {
	s, _ := newTestServer(t)
	m := addProviderModel(t, s, "http://x.invalid", "m", func(m *Model) { m.MaxConcurrent = 2 })

	if !s.store.TryAcquire(m.ID) || !s.store.TryAcquire(m.ID) {
		t.Fatal("前两个名额应能取到")
	}
	if s.store.TryAcquire(m.ID) {
		t.Fatal("超出上限应拒绝")
	}
	s.store.Release(m.ID)
	if !s.store.TryAcquire(m.ID) {
		t.Fatal("释放后应能再取")
	}
	if got := s.store.Inflight()[m.ID]; got != 2 {
		t.Fatalf("在途数应为 2, got %d", got)
	}
	s.store.Release(m.ID)
	s.store.Release(m.ID)
	_, exists := s.store.Inflight()[m.ID]
	if exists {
		t.Fatal("归零后应删除键，避免 map 增长")
	}
}

// 无上限（0）时不应拦截
func TestNoConcurrentLimitWhenZero(t *testing.T) {
	s, _ := newTestServer(t)
	m := addProviderModel(t, s, "http://x.invalid", "m", nil)
	for i := 0; i < 50; i++ {
		if !s.store.TryAcquire(m.ID) {
			t.Fatalf("无上限时第 %d 次应成功", i+1)
		}
	}
}

// 并行已满的模型不进入候选，请求自动落到后续候选
func TestFullModelSkippedByCandidates(t *testing.T) {
	s, _ := newTestServer(t)
	full := addProviderModel(t, s, "http://a.invalid", "m", func(m *Model) {
		m.Priority = 20
		m.MaxConcurrent = 1
	})
	backup := addProviderModel(t, s, "http://b.invalid", "m", func(m *Model) {
		m.Priority = 10
		m.MaxConcurrent = 1
	})

	cands := s.store.Candidates("m", time.Now())
	if len(cands) != 2 {
		t.Fatalf("初始应有 2 个候选, got %d", len(cands))
	}
	s.store.TryAcquire(full.ID) // 占满高优先级模型
	cands = s.store.Candidates("m", time.Now())
	if len(cands) != 1 || cands[0].Model.ID != backup.ID {
		t.Fatalf("满额模型应被剔除，只剩备用候选: %+v", cands)
	}
	// 全部占满时无候选
	s.store.TryAcquire(backup.ID)
	if cands = s.store.Candidates("m", time.Now()); len(cands) != 0 {
		t.Fatalf("全部满额应无候选, got %d", len(cands))
	}
}

// 端到端：并行满时新请求改走后续候选，且请求结束后名额被释放
func TestConcurrentLimitFailover(t *testing.T) {
	release := make(chan struct{})
	var hitA, hitB int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitA, 1)
		<-release // 挂住第一个请求，保持名额被占用
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitB, 1)
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upB.Close()

	s, app := newTestServer(t)
	a := addProviderModel(t, s, upA.URL, "m", func(m *Model) {
		m.Priority = 20
		m.MaxConcurrent = 1
	})
	addProviderModel(t, s, upB.URL, "m", func(m *Model) { m.Priority = 10 })

	// 第一个请求占住 A（会阻塞在 release 上）
	done := make(chan struct{})
	go func() {
		defer close(done)
		postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`)
	}()
	// 等 A 的在途数变成 1
	deadline := time.Now().Add(2 * time.Second)
	for s.store.Inflight()[a.ID] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("第一个请求未占用名额")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 此时 A 已满，第二个请求应落到 B
	if resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatalf("第二个请求应成功走 B: %d %s", resp.StatusCode, body)
	}
	if atomic.LoadInt32(&hitB) == 0 {
		t.Fatal("第二个请求应打到备用上游 B")
	}

	close(release)
	<-done
	// 第一个请求结束后名额应释放
	deadline = time.Now().Add(2 * time.Second)
	for s.store.Inflight()[a.ID] != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("名额未释放, inflight=%d", s.store.Inflight()[a.ID])
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&hitA) != 1 {
		t.Fatalf("A 应只被调用 1 次, got %d", atomic.LoadInt32(&hitA))
	}
}

// 上游失败路径也要释放名额，否则名额会泄漏
func TestConcurrentSlotReleasedOnFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	m := addProviderModel(t, s, up.URL, "m", func(m *Model) { m.MaxConcurrent = 1 })

	for i := 0; i < 3; i++ {
		if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 502 {
			t.Fatalf("第 %d 次应 502, got %d", i+1, resp.StatusCode)
		}
		if got := s.store.Inflight()[m.ID]; got != 0 {
			t.Fatalf("第 %d 次失败后名额应已释放, got %d", i+1, got)
		}
	}
}

// 后台状态里带 inflight 字段，前端才能展示并行占用
func TestStateIncludesInflight(t *testing.T) {
	s, app := newTestServer(t)
	m := addProviderModel(t, s, "http://x.invalid", "m", func(m *Model) { m.MaxConcurrent = 3 })
	s.store.TryAcquire(m.ID)

	resp, body := postJSONWithHeader(t, app, "GET", "/api/state", "", "X-Admin-Token", "")
	if resp.StatusCode != 200 {
		t.Fatalf("应 200, got %d", resp.StatusCode)
	}
	var v struct {
		Models []struct {
			ID            string `json:"id"`
			MaxConcurrent int    `json:"max_concurrent"`
			Inflight      int    `json:"inflight"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Models) != 1 || v.Models[0].Inflight != 1 || v.Models[0].MaxConcurrent != 3 {
		t.Fatalf("状态应带 inflight/max_concurrent: %+v", v.Models)
	}
}

// 会话建立要记日志，含选中理由、候选顺序与被剔除原因
func TestSessionLogNewBinding(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	// 高优先级模型设静音，应该被剔除
	now := time.Now()
	from := now.Add(-time.Hour).Format("15:04")
	to := now.Add(time.Hour).Format("15:04")
	addProviderModel(t, s, up.URL, "m", func(m *Model) {
		m.Priority = 99
		m.Mutes = []MuteWindow{{From: from, To: to}}
	})
	addProviderModel(t, s, up.URL, "m", func(m *Model) { m.Priority = 10 })

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-1"); resp.StatusCode != 200 {
		t.Fatalf("应 200, got %d %s", resp.StatusCode, body)
	}

	s.store.Lock()
	var sess *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = l
		}
	}
	s.store.Unlock()
	if sess == nil {
		t.Fatal("应记录一条会话日志")
	}
	if sess.Model != "m" || sess.SessionID != "sess-1" || sess.Requested != "m" {
		t.Errorf("会话日志字段不对: %+v", sess)
	}
	if sess.Switched {
		t.Error("首次建立不应标记为会话换模型")
	}
	if !strings.Contains(sess.Reason, "按优先级") {
		t.Errorf("理由应说明按优先级选中: %q", sess.Reason)
	}
	if len(sess.Candidates) != 1 {
		t.Errorf("候选应只剩 1 个（静音的被剔除）: %v", sess.Candidates)
	}
	if len(sess.Skipped) != 1 || !strings.Contains(sess.Skipped[0].Reason, "静音") {
		t.Errorf("应记录被静音剔除的条目: %+v", sess.Skipped)
	}
}

// 同一会话的后续请求复用绑定，不应重复记录日志（只记一次）
func TestSessionLogOnlyOncePerSession(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)

	for i := 0; i < 5; i++ {
		if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
			"X-Session-ID", "sess-once"); resp.StatusCode != 200 {
			t.Fatalf("第 %d 次应 200, got %d %s", i+1, resp.StatusCode, body)
		}
	}
	s.store.Lock()
	var sess []*LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = append(sess, l)
		}
	}
	s.store.Unlock()
	if len(sess) != 1 {
		t.Fatalf("5 次同会话请求只应记 1 条日志, got %d", len(sess))
	}
	if sess[0].Switched {
		t.Error("首条应为新会话（switched=false）")
	}
	if !strings.Contains(sess[0].Reason, "新会话") {
		t.Errorf("理由应为新会话: %q", sess[0].Reason)
	}
}

// 会话中途换模型（原绑定不可用）要额外记一条，并标记 switched
func TestSessionLogRecordsSwitch(t *testing.T) {
	var upAHealthy atomic.Bool
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !upAHealthy.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upB.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, upA.URL, "m", func(m *Model) { m.Priority = 20 })
	addProviderModel(t, s, upB.URL, "m", func(m *Model) { m.Priority = 10 })

	// 第一次：A 正常，会话绑定到 A
	upAHealthy.Store(true)
	if resp, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-sw"); resp.StatusCode != 200 {
		t.Fatalf("首次应 200, got %d", resp.StatusCode)
	}

	// 第二次：A 开始报错，会话应切到 B 并记录切换
	upAHealthy.Store(false)
	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-sw"); resp.StatusCode != 200 {
		t.Fatalf("切换后应 200, got %d %s", resp.StatusCode, body)
	}

	s.store.Lock()
	var sess []*LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = append(sess, l)
		}
	}
	s.store.Unlock()
	if len(sess) != 2 {
		t.Fatalf("建立 + 切换应共 2 条, got %d", len(sess))
	}
	if sess[0].Switched {
		t.Error("第一条应为新会话")
	}
	if !sess[1].Switched {
		t.Error("第二条应标记为会话换模型")
	}
	if !strings.Contains(sess[1].Reason, "会话内更换模型") {
		t.Errorf("理由应说明会话内换模型: %q", sess[1].Reason)
	}
	if !strings.Contains(sess[1].Message, "会话换模型") {
		t.Errorf("消息应体现换模型: %q", sess[1].Message)
	}
}

// 高优先级候选尝试失败降级到下一个时，会话日志要记录失败原因与降级说明
func TestSessionLogRecordsAttemptFailures(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upB.Close()

	s, app := newTestServer(t)
	ma := addProviderModel(t, s, upA.URL, "m", func(m *Model) { m.Priority = 20 })
	addProviderModel(t, s, upB.URL, "m", func(m *Model) { m.Priority = 10 })

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-failover"); resp.StatusCode != 200 {
		t.Fatalf("降级后应 200, got %d %s", resp.StatusCode, body)
	}
	s.store.Lock()
	var sess *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = l
		}
	}
	s.store.Unlock()
	if sess == nil {
		t.Fatal("应记录会话日志")
	}
	if len(sess.Candidates) != 2 {
		t.Errorf("候选应保留完整顺序: %v", sess.Candidates)
	}
	if len(sess.Skipped) != 1 {
		t.Fatalf("应记录尝试失败的候选: %+v", sess.Skipped)
	}
	sk := sess.Skipped[0]
	if sk.ModelID != ma.ID || !strings.Contains(sk.Reason, "尝试失败") || !strings.Contains(sk.Reason, "HTTP 500") {
		t.Errorf("失败原因应指向高优先级候选并带上游错误: %+v", sk)
	}
	if !strings.Contains(sess.Reason, "1 个候选尝试失败已降级") {
		t.Errorf("理由应说明降级: %q", sess.Reason)
	}
}

// 请求时并行名额已被占满而让位的候选，会话日志要记录让位原因
// （名额已占时在规则筛选阶段即被剔除并带原因；循环内 TryAcquire 失败是并发
// 竞态的兜底路径，同样会以「尝试时并行已满」记录，此处覆盖规则路径）
func TestSessionLogRecordsConcurrencyYield(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	ma := addProviderModel(t, s, up.URL, "m", func(m *Model) { m.Priority = 20; m.MaxConcurrent = 1 })
	addProviderModel(t, s, up.URL, "m", func(m *Model) { m.Priority = 10 })
	if !s.store.TryAcquire(ma.ID) { // 预占高优先级候选唯一的并行名额
		t.Fatal("预占名额失败")
	}
	defer s.store.Release(ma.ID)

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-yield"); resp.StatusCode != 200 {
		t.Fatalf("让位后应 200, got %d %s", resp.StatusCode, body)
	}
	s.store.Lock()
	var sess *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = l
		}
	}
	s.store.Unlock()
	if sess == nil {
		t.Fatal("应记录会话日志")
	}
	found := false
	for _, sk := range sess.Skipped {
		if sk.ModelID == ma.ID && strings.Contains(sk.Reason, "并行已满") {
			found = true
		}
	}
	if !found {
		t.Errorf("应记录并行让位原因: %+v", sess.Skipped)
	}
	if !strings.Contains(sess.Reason, "并行已满") {
		t.Errorf("理由应点出让位原因: %q", sess.Reason)
	}
}

// auto 虚拟模型的理由应说明是全局最高优先级
func TestSessionLogAutoModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "real-model", func(m *Model) { m.Priority = 7 })

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"auto"}`,
		"X-Session-ID", "sess-auto"); resp.StatusCode != 200 {
		t.Fatalf("应 200, got %d %s", resp.StatusCode, body)
	}
	s.store.Lock()
	var sess *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = l
		}
	}
	s.store.Unlock()
	if sess == nil {
		t.Fatal("应记录会话日志")
	}
	if sess.Model != "real-model" || sess.Requested != "auto" {
		t.Errorf("应记录实际模型与原始请求名: %+v", sess)
	}
	if sess.Protocol != "chat" {
		t.Errorf("chat 入口应记录 protocol=chat: %+v", sess)
	}
	if !strings.Contains(sess.Reason, "auto") {
		t.Errorf("理由应提到 auto: %q", sess.Reason)
	}
}

// 会话日志要记录接入协议，区分 chat completions 与 responses
func TestSessionLogProtocol(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"x1","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-chat"); resp.StatusCode != 200 {
		t.Fatalf("chat 应 200, got %d %s", resp.StatusCode, body)
	}
	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/responses", `{"model":"m"}`,
		"X-Session-ID", "sess-resp"); resp.StatusCode != 200 {
		t.Fatalf("responses 应 200, got %d %s", resp.StatusCode, body)
	}

	got := map[string]string{}
	s.store.Lock()
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			got[l.SessionID] = l.Protocol
		}
	}
	s.store.Unlock()
	if got["sess-chat"] != "chat" || got["sess-resp"] != "responses" {
		t.Errorf("会话日志协议记录不正确: %+v", got)
	}
}

// 并行已满让位的模型，要在理由里点出来
func TestSessionLogMentionsFullModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	full := addProviderModel(t, s, up.URL, "m", func(m *Model) {
		m.Priority = 50
		m.MaxConcurrent = 1
	})
	addProviderModel(t, s, up.URL, "m", func(m *Model) { m.Priority = 10 })
	s.store.TryAcquire(full.ID) // 占满高优先级模型

	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "sess-full"); resp.StatusCode != 200 {
		t.Fatalf("应 200, got %d %s", resp.StatusCode, body)
	}
	s.store.Lock()
	var sess *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindSession {
			sess = l
		}
	}
	s.store.Unlock()
	if sess == nil {
		t.Fatal("应记录会话日志")
	}
	if !strings.Contains(sess.Reason, "并行已满") {
		t.Errorf("理由应说明有模型因并行已满让位: %q", sess.Reason)
	}
	if len(sess.Skipped) != 1 || !strings.Contains(sess.Skipped[0].Reason, "并行已满") {
		t.Errorf("应记录并行已满的剔除项: %+v", sess.Skipped)
	}
}

// CandidatesDetailed 的剔除原因要覆盖静音、超限、并行满、Provider 缺失
func TestCandidatesDetailedSkipReasons(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now()
	from := now.Add(-time.Hour).Format("15:04")
	to := now.Add(time.Hour).Format("15:04")

	muted := addProviderModel(t, s, "http://a.invalid", "m", func(m *Model) {
		m.Priority = 40
		m.Mutes = []MuteWindow{{From: from, To: to}}
	})
	limited := addProviderModel(t, s, "http://b.invalid", "m", func(m *Model) {
		m.Priority = 30
		m.DailyLimit = 1
	})
	capacity := addProviderModel(t, s, "http://c.invalid", "m", func(m *Model) {
		m.Priority = 20
		m.MaxConcurrent = 1
	})
	ok := addProviderModel(t, s, "http://d.invalid", "m", func(m *Model) { m.Priority = 10 })

	// 造出「已达每日上限」
	s.store.AddUsage(&Model{ProviderID: limited.ProviderID, Name: limited.Name}, "", Usage{Prompt: 5})
	s.store.TryAcquire(capacity.ID)

	cands, skips := s.store.CandidatesDetailed("m", time.Now(), false)
	if len(cands) != 1 || cands[0].Model.ID != ok.ID {
		t.Fatalf("应只剩 1 个可用候选: %+v", cands)
	}
	if len(skips) != 3 {
		t.Fatalf("应有 3 个剔除项, got %d: %+v", len(skips), skips)
	}
	byID := map[string]string{}
	for _, sk := range skips {
		byID[sk.ModelID] = sk.Reason
	}
	if !strings.Contains(byID[muted.ID], "静音") {
		t.Errorf("静音原因不对: %q", byID[muted.ID])
	}
	if !strings.Contains(byID[limited.ID], "每日") {
		t.Errorf("超限原因不对: %q", byID[limited.ID])
	}
	if !strings.Contains(byID[capacity.ID], "并行已满") {
		t.Errorf("并行原因不对: %q", byID[capacity.ID])
	}
}

// 上游返回 429 后，该模型条目应被自动静音，后续请求直接跳过它
func TestMuteOn429(t *testing.T) {
	var hits int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded"}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upB.Close()

	s, app := newTestServer(t)
	a := addProviderModel(t, s, upA.URL, "m", func(m *Model) { m.Priority = 20 })
	addProviderModel(t, s, upB.URL, "m", func(m *Model) { m.Priority = 10 })

	// 第一次：A 返回 429 → 自动静音，请求改走 B 并成功
	if resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatalf("应透过 B 成功, got %d %s", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("A 应被调用 1 次, got %d", got)
	}

	// A 应处于静音期
	s.store.Lock()
	var muted *Model
	for _, m := range s.store.Models {
		if m.ID == a.ID {
			muted = m
		}
	}
	s.store.Unlock()
	if muted == nil || muted.MutedUntil.IsZero() {
		t.Fatal("A 应被设置 MutedUntil")
	}
	if d := time.Until(muted.MutedUntil); d < 4*time.Hour || d > 5*time.Hour {
		t.Fatalf("静音时长应约为 5 小时, got %v", d)
	}

	// 后续多次请求都不应再打到 A
	for i := 0; i < 3; i++ {
		if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
			t.Fatalf("第 %d 次应成功", i+2)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("静音后 A 不应再被调用, got %d 次", got)
	}
}

// 429 静音期间，会话绑定也不能绕过（否则会话会持续打一个限流的上游）
func TestMutedModelSkippedBySessionBinding(t *testing.T) {
	var hitsA int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upB.Close()

	s, app := newTestServer(t)
	a := addProviderModel(t, s, upA.URL, "m", func(m *Model) { m.Priority = 20 })
	addProviderModel(t, s, upB.URL, "m", func(m *Model) { m.Priority = 10 })

	// 会话绑定到 A
	if resp, _ := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "s1"); resp.StatusCode != 200 {
		t.Fatal("首次应成功")
	}
	if atomic.LoadInt32(&hitsA) != 1 {
		t.Fatal("首次应打到 A")
	}

	// 手动把 A 静音（模拟它刚返回过 429）
	s.store.MuteOn429(a.ID, 5*time.Hour, "test")

	// 同会话再请求：绑定已失效，应改走 B
	if resp, body := postJSONWithHeader(t, app, "POST", "/v1/chat/completions", `{"model":"m"}`,
		"X-Session-ID", "s1"); resp.StatusCode != 200 {
		t.Fatalf("应透过 B 成功, got %d %s", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Fatalf("静音后 A 不应再被调用, got %d", got)
	}
}

// 静音到期自动恢复；已在静音期内再次 429 只会延长，不会缩短
func TestMutedUntilExpiryAndExtend(t *testing.T) {
	s, _ := newTestServer(t)
	m := addProviderModel(t, s, "http://x.invalid", "m", nil)

	// 到期即恢复：直接设一个已过去的时间
	s.store.Lock()
	s.store.Models[0].MutedUntil = time.Now().Add(-time.Minute)
	s.store.Unlock()
	if r := s.store.SkipReasonFor(s.store.Models[0], time.Now(), time.Now().Format("2006-01-02")); r != "" {
		t.Fatalf("静音到期后应可用, got %q", r)
	}
	if _, ok := s.store.CandidateByID(m.ID); !ok {
		t.Fatal("静音到期后应能按 ID 取到候选")
	}

	// 再次 429 延长到 5 小时
	until1, ok := s.store.MuteOn429(m.ID, 5*time.Hour, "r1")
	if !ok {
		t.Fatal("应静音成功")
	}
	// 已有更长静音时，用更短的时长不应缩短
	s.store.Lock()
	s.store.Models[0].MutedUntil = time.Now().Add(10 * time.Hour)
	s.store.Unlock()
	until2, _ := s.store.MuteOn429(m.ID, 1*time.Hour, "r2")
	if !until2.After(until1) || time.Until(until2) < 9*time.Hour {
		t.Fatalf("不应缩短已有静音: until1=%v until2=%v", until1, until2)
	}
}

// 手动解除静音接口
func TestClearMuteAPI(t *testing.T) {
	s, app := newTestServer(t)
	m := addProviderModel(t, s, "http://x.invalid", "m", nil)
	s.store.MuteOn429(m.ID, 5*time.Hour, "test")

	req, _ := http.NewRequest("DELETE", app.URL+"/api/models/"+m.ID+"/mute", nil)
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("解除静音应 200, got %d", resp.StatusCode)
	}
	s.store.Lock()
	mu := s.store.Models[0].MutedUntil
	s.store.Unlock()
	if !mu.IsZero() {
		t.Fatalf("解除后 MutedUntil 应归零, got %v", mu)
	}

	// 再次解除应 400（本来就没静音）
	req2, _ := http.NewRequest("DELETE", app.URL+"/api/models/"+m.ID+"/mute", nil)
	resp2, _ := app.Client().Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("未静音时解除应 400, got %d", resp2.StatusCode)
	}
}

// 429 要记一条 muted 日志，且 429 模型不参与候选
func TestMuteOn429LogsAndSkips(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	m := addProviderModel(t, s, up.URL, "m", nil)

	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 502 {
		t.Fatalf("无备用时应 502, got %d", resp.StatusCode)
	}

	s.store.Lock()
	var mutedLog *LogEntry
	for _, l := range s.store.Logs {
		if l.Kind == kindMuted {
			mutedLog = l
		}
	}
	s.store.Unlock()
	if mutedLog == nil {
		t.Fatal("应记录一条 muted 日志")
	}
	if mutedLog.Status != 429 || !strings.Contains(mutedLog.Message, "自动静音") {
		t.Errorf("muted 日志不对: %+v", mutedLog)
	}

	// 已静音 → 该模型不进入候选
	cands, skips := s.store.CandidatesDetailed("m", time.Now(), false)
	if len(cands) != 0 {
		t.Fatalf("静音模型不应进入候选, got %d", len(cands))
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "429") {
		t.Fatalf("剔除原因应说明 429: %+v", skips)
	}
	_ = m
}

// 上游 usage 中 completion_tokens_details.cached_tokens 可能排在 prompt 分项之前
// （WorkBuddy/DeepSeek 网关的实测形状），解析必须只认 prompt/input 的缓存分项。
func TestCachedTokensPreferPromptDetails(t *testing.T) {
	tail := []byte("data: " + `{"choices":[{"delta":{"content":""},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":3008,"completion_tokens":1,"total_tokens":3009,` +
		`"completion_tokens_details":{"reasoning_tokens":0,"cached_tokens":0},` +
		`"prompt_tokens_details":{"reasoning_tokens":0,"cached_tokens":2816},` +
		`"prompt_cache_hit_tokens":2816,"prompt_cache_miss_tokens":192,` +
		`"cache_read_input_tokens":0,"cached_tokens":0}}` + "\n\n")
	u := wire.ExtractStreamUsage(tail)
	if u.Prompt != 3008 || u.Completion != 1 || u.Cached != 2816 {
		t.Fatalf("流式缓存优先取 prompt 分项失败: %+v", u)
	}
}

// usage 缓存字段的回落顺序：prompt/input 分项 → DeepSeek → Anthropic → 顶层；
// completion_tokens_details.cached_tokens 永远不当作提示缓存。
func TestParseUsageObjectCacheFallbacks(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Usage
	}{
		{"prompt_details", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}`,
			Usage{Prompt: 100, Completion: 10, Cached: 80}},
		{"input_details", `{"input_tokens":107,"output_tokens":36,"input_tokens_details":{"cached_tokens":64}}`,
			Usage{Prompt: 107, Completion: 36, Cached: 64}},
		{"deepseek", `{"prompt_tokens":50,"completion_tokens":5,"prompt_cache_hit_tokens":40}`,
			Usage{Prompt: 50, Completion: 5, Cached: 40}},
		{"anthropic", `{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":70}`,
			Usage{Prompt: 20, Completion: 5, Cached: 70}},
		{"top_level", `{"prompt_tokens":30,"completion_tokens":5,"cached_tokens":12}`,
			Usage{Prompt: 30, Completion: 5, Cached: 12}},
		{"completion_details_only", `{"prompt_tokens":30,"completion_tokens":5,"completion_tokens_details":{"cached_tokens":9}}`,
			Usage{Prompt: 30, Completion: 5, Cached: 0}},
		{"explicit_zero_no_fallback", `{"prompt_tokens":30,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":9}`,
			Usage{Prompt: 30, Completion: 5, Cached: 0}},
	}
	for _, tc := range cases {
		if got := wire.ParseUsage([]byte(tc.raw)); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// 非流式透传同样按新解析器记账
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[],"usage":{"completion_tokens":1,"completion_tokens_details":{"cached_tokens":0},`+
			`"prompt_tokens":3008,"prompt_tokens_details":{"cached_tokens":2816}}}`)
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)
	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatal("请求失败")
	}
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	json.Unmarshal(s.store.SnapshotJSON(), &st)
	if len(st.Usage) != 1 || st.Usage[0].CachedTokens != 2816 || st.Usage[0].PromptTokens != 3008 {
		t.Fatalf("非流式缓存记账错误: %+v", st.Usage)
	}
}

// usage:null 或尾部被截断时不应把正文里的 "usage" 文本误当用量。
func TestStreamUsageIgnoresContentAndTruncatedJSON(t *testing.T) {
	if u := wire.ExtractStreamUsage([]byte(`..."usage":null`)); u != (Usage{}) {
		t.Fatalf("usage:null 应记 0: %+v", u)
	}
	truncated := []byte(`..."usage":{"prompt_tokens":12,"completion`) // usage 对象未闭合
	if u := wire.ExtractStreamUsage(truncated); u != (Usage{}) {
		t.Fatalf("截断的 usage 应记 0: %+v", u)
	}
}

// 逆序扫描应跳过 usage:null 的帧与正文里的 "usage" 文本；尾部截断时仍能取到完整 usage 对象。
func TestStreamUsageReverseScan(t *testing.T) {
	tail := []byte("data: " + `{"choices":[{"delta":{"content":"example usage: {\"cached_tokens\":999}"}}]}` + "\n\n" +
		"data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":7}}}` + "\n\n" +
		"data: " + `{"choices":[],"usage":{}}` + "\n\n" +
		"data: " + `{"choices":[],"usage":null}` + "\n\n" +
		"data: [DONE]\n\n")
	if u := wire.ExtractStreamUsage(tail); u != (Usage{Prompt: 10, Completion: 1, Cached: 7}) {
		t.Fatalf("逆序扫描取 usage 错误: %+v", u)
	}

	truncated := []byte(`data: {"type":"response.completed","response":{"id":"x","output":[{"type":"message"}],"usage":{"input_tokens":107,"input_tokens_details":{"cached_tokens":64},"output_tokens":36}}`)
	if u := wire.ExtractStreamUsage(truncated); u != (Usage{Prompt: 107, Completion: 36, Cached: 64}) {
		t.Fatalf("截断事件的 usage 解析错误: %+v", u)
	}
}

// 流式 chat completions 经代理后，缓存命中应记 prompt 分项而不是 completion 分项。
func TestStreamCachedTokensThroughProxy(t *testing.T) {
	sse := "data: " + `{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: " + `{"choices":[],"usage":{"prompt_tokens":3008,"completion_tokens":1,"total_tokens":3009,` +
		`"completion_tokens_details":{"reasoning_tokens":0,"cached_tokens":0},` +
		`"prompt_tokens_details":{"reasoning_tokens":0,"cached_tokens":2816},` +
		`"prompt_cache_hit_tokens":2816,"cached_tokens":0}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer up.Close()

	s, app := newTestServer(t)
	addProviderModel(t, s, up.URL, "m", nil)
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m","stream":true}`)
	if resp.StatusCode != 200 || body != sse {
		t.Fatalf("SSE 应原样透传: %d %q", resp.StatusCode, body)
	}
	var st struct {
		Usage []*UsageStat `json:"usage"`
	}
	for i := 0; i < 50; i++ {
		json.Unmarshal(s.store.SnapshotJSON(), &st)
		if len(st.Usage) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(st.Usage) != 1 || st.Usage[0].CachedTokens != 2816 ||
		st.Usage[0].PromptTokens != 3008 || st.Usage[0].CompletionTokens != 1 {
		t.Fatalf("流式缓存记账错误: %+v", st.Usage)
	}
}

// response.completed 事件超过 8KB 尾部窗口时，残片没有 data: 前缀也要能取出 usage。
func TestStreamUsageTruncatedEventWithoutPrefix(t *testing.T) {
	fragment := []byte(`"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],` +
		`"usage":{"input_tokens":107,"input_tokens_details":{"cached_tokens":64},"output_tokens":36},"status":"completed"}}`)
	if u := wire.ExtractStreamUsage(fragment); u != (Usage{Prompt: 107, Completion: 36, Cached: 64}) {
		t.Fatalf("无 data: 前缀的残片解析错误: %+v", u)
	}
}
