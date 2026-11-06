package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	if resp, _ := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`); resp.StatusCode != 200 {
		t.Fatalf("第一次应成功（10 tokens 未超限）, got %d", resp.StatusCode)
	}
	resp, body := postJSON(t, app, "/v1/chat/completions", `{"model":"m"}`)
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
	addProviderModel(t, s, up.URL, "aaa", nil)

	resp, _ := postJSON(t, app, "/v1/models", "") // POST 应 405，换 GET
	if resp.StatusCode != 405 {
		t.Fatalf("POST /v1/models 应 405, got %d", resp.StatusCode)
	}
	r2, err := app.Client().Get(app.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	var v struct {
		Data []struct {
			ID string
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	// 第一个恒为 auto 虚拟模型，其后为真实模型名去重排序
	if len(v.Data) != 3 || v.Data[0].ID != "auto" || v.Data[1].ID != "aaa" || v.Data[2].ID != "zzz" {
		t.Errorf("模型列表不正确: %s", b)
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
	reqBody := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	if _, body := postJSON(t, app, "/v1/chat/completions", reqBody); body != `{"ok":"A"}` {
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
	if _, body := postJSON(t, app, "/v1/chat/completions", reqBody); body != `{"ok":"B"}` {
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
	s := &Server{sessions: map[string]session{"k": {modelID: "m1", expires: time.Now().Add(-time.Minute)}}}
	if _, ok := s.getSession("k"); ok {
		t.Error("过期会话应失效")
	}
	s.setSession("k", "m2")
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
	if u := extractStreamUsage(respStyle); u.Prompt != 107 || u.Completion != 36 {
		t.Errorf("responses 风格提取错误: %d/%d", u.Prompt, u.Completion)
	}
	// chat 风格
	chatStyle := []byte(`...data: {"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	if u := extractStreamUsage(chatStyle); u.Prompt != 5 || u.Completion != 6 {
		t.Errorf("chat 风格提取错误: %d/%d", u.Prompt, u.Completion)
	}
	// 无 usage / usage:null
	if u := extractStreamUsage([]byte("data: [DONE]")); u.Prompt != 0 || u.Completion != 0 {
		t.Errorf("无 usage 应为 0: %d/%d", u.Prompt, u.Completion)
	}
	if u := extractStreamUsage([]byte(`{"usage":null}`)); u.Prompt != 0 || u.Completion != 0 {
		t.Errorf("usage:null 应为 0: %d/%d", u.Prompt, u.Completion)
	}
}

func TestCachedTokens(t *testing.T) {
	// 流式尾部嗅探应提取缓存命中量
	tail := []byte(`data: {"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}}`)
	if got := extractStreamUsage(tail); got.Prompt != 100 || got.Cached != 80 || got.Completion != 10 {
		t.Errorf("流式缓存提取错误: %+v", got)
	}
	// 无缓存字段时 Cached 为 0
	tail2 := []byte(`data: {"usage":{"prompt_tokens":50,"completion_tokens":5}}`)
	if got := extractStreamUsage(tail2); got.Prompt != 50 || got.Cached != 0 {
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
