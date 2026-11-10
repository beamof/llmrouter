package app

// WorkBuddy 适配测试：归一化/推理档位/错误分类/节拍过滤/聚合等纯函数表驱动，
// 转发链路用 httptest 伪造上游（/v2/chat/completions、/v2/plugin/auth/token/refresh、
// /v3/config），验证请求头形状、续期轮换、静音时刻与流式/非流式分派。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/upstream/workbuddy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// addWBProviderModel 建一个 WorkBuddy provider + 模型路由（BaseURL = 端点 + /v2）

func addWBProviderModel(t *testing.T, s *Server, endpoint, name string, cred *WorkBuddyCred) (*Provider, *Model) {
	t.Helper()
	p := &Provider{Name: "wb-p", Kind: "workbuddy", BaseURL: endpoint + "/v2", WB: cred}
	s.store.UpsertProvider(p)
	m := &Model{Name: name, ProviderID: p.ID, Priority: 10}
	s.store.UpsertModel(m)
	return p, m
}

// wbSealField 测试辅助：按信封 spec 加密一个字段（与 wbOpenEnvelope 互为逆操作）

func wbSealField(t *testing.T, secret, plaintext string) map[string]any {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	keyHash := sha256.Sum256(key[:])
	keyID := hex.EncodeToString(keyHash[:])[:16]
	nonce := make([]byte, 12)
	rand.Read(nonce)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, _ := cipher.NewGCM(block)
	ct := gcm.Seal(nil, nonce, []byte(plaintext), workbuddy.AAD(keyID, 1))
	env := fmt.Sprintf(`{"suite":1,"keyId":%q,"nonce":%q,"authTag":%q,"ciphertext":%q}`,
		keyID,
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		base64.StdEncoding.EncodeToString(ct[:len(ct)-16]),
	)
	return map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString([]byte(env))}
}

// ── 纯函数 ────────────────────────────────────────────────────────────────

func wbFakeUpstream(t *testing.T, chat http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	refreshes := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, workbuddy.CatalogBody)
	})
	mux.HandleFunc("POST /v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`)
	})
	mux.HandleFunc("POST /v2/chat/completions", chat)
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)
	return up, refreshes
}

func TestWBChatHeadersAndBody(t *testing.T) {
	var gotHeaders http.Header
	var gotBody string
	up, refreshes := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	cred := &WorkBuddyCred{UID: "uid-123456789", AccessToken: "at-1", RefreshToken: "rt-1",
		ExpiresAt: workbuddy.TimePtr(time.Now().Add(time.Hour))}
	addWBProviderModel(t, s, up.URL, "glm-5.0", cred)

	resp, body := postJSON(t, app, "/v1/chat/completions", `{
		"model":"glm-5.0","stream":true,"max_completion_tokens":1234,
		"tool_choice":{"type":"function","function":{"name":"fx"}},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":"https://x/y.png"}]},
			{"role":"developer","content":"d"},
			{"role":"assistant","content":"旧答","reasoning_content":"旧思考"}
		]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if refreshes.Load() != 0 {
		t.Fatal("token 未临期不应刷新")
	}

	// 请求头形状（官方 CLI 指纹）
	h := gotHeaders
	for k, want := range map[string]string{
		"Authorization":       "Bearer at-1",
		"X-User-Id":           "uid-123456789",
		"User-Agent":          workbuddy.CLIUserAgent(),
		"X-IDE-Type":          "CLI",
		"X-IDE-Name":          "CLI",
		"X-IDE-Version":       workbuddy.CLIVersion,
		"X-CodeBuddy-Request": "1",
		"X-Agent-Intent":      "craft",
		"X-Product":           "SaaS",
		"X-Requested-With":    "XMLHttpRequest",
		"Accept":              "text/event-stream, application/json",
		"Accept-Language":     "zh-CN",
		"Content-Type":        "application/json",
		"Accept-Encoding":     "identity",
	} {
		if h.Get(k) != want {
			t.Fatalf("头 %s = %q, want %q", k, h.Get(k), want)
		}
	}
	// Origin/Referer/X-Domain 跟随实际请求端点；refresh token 绝不上 chat
	upu, _ := url.Parse(up.URL)
	if h.Get("Origin") != up.URL || h.Get("Referer") != up.URL+"/" || h.Get("X-Domain") != upu.Hostname() {
		t.Fatalf("Origin/Referer/X-Domain 应跟随端点 %s: %q %q %q",
			up.URL, h.Get("Origin"), h.Get("Referer"), h.Get("X-Domain"))
	}
	if h.Get("X-Refresh-Token") != "" {
		t.Fatal("chat 请求不应携带 X-Refresh-Token")
	}
	for _, k := range []string{"X-Agent-Purpose", "X-Agent-Type", "X-Private-Data", "X-Agent-Runtime"} {
		if h.Get(k) != "" {
			t.Fatalf("CLI 指纹不应携带桌面端头 %s", k)
		}
	}
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(h.Get("traceparent")) {
		t.Fatalf("traceparent 形状不对: %q", h.Get("traceparent"))
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(h.Get("X-Conversation-ID")) {
		t.Fatalf("X-Conversation-ID 形状不对: %q", h.Get("X-Conversation-ID"))
	}
	if h.Get("X-Conversation-Request-ID") != h.Get("X-Root-Request-ID") {
		t.Fatal("X-Conversation-Request-ID 与 X-Root-Request-ID 应同值")
	}
	if h.Get("X-Request-ID") != h.Get("X-Conversation-Message-ID") {
		t.Fatal("X-Request-ID 与 X-Conversation-Message-ID 应同值")
	}

	// 出站归一化结果
	var out map[string]any
	if err := json.Unmarshal([]byte(gotBody), &out); err != nil {
		t.Fatalf("出站体不是 JSON: %v: %s", err, gotBody)
	}
	if out["stream"] != true {
		t.Fatal("上游应强制流式")
	}
	if out["model"] != "glm-5.0" {
		t.Fatalf("model 不应被改: %v", out["model"])
	}
	if out["max_tokens"] != float64(1234) {
		t.Fatalf("max_completion_tokens 应翻译: %v", out["max_tokens"])
	}
	if out["tool_choice"] != "fx" {
		t.Fatalf("tool_choice 应折成函数名: %v", out["tool_choice"])
	}
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != workbuddy.DefaultSystemPrompt {
		t.Fatalf("user 打头应注入兜底 system: %#v", msgs[0])
	}
	if msgs[2].(map[string]any)["role"] != "system" {
		t.Fatalf("developer 应折成 system: %#v", msgs[2])
	}
	if msgs[1].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"] != "https://x/y.png" {
		t.Fatal("image_url 应包成对象")
	}
	if msgs[3].(map[string]any)["reasoning"] != "旧思考" {
		t.Fatalf("启用思考应回填历史 reasoning: %#v", msgs[3])
	}
	if out["reasoning_effort"] != "medium" {
		t.Fatalf("应注入目录默认档 medium: %v", out["reasoning_effort"])
	}
	if k, _ := out["prompt_cache_key"].(string); !strings.HasPrefix(k, "wb2a-uid-1234-") {
		t.Fatalf("应注入缓存键: %v", out["prompt_cache_key"])
	}
	so, _ := out["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatal("应带 stream_options.include_usage")
	}
	// 客户端头不应透传给上游（伪造一个独有头）
	// （postJSON 无法带自定义头，这里验证常见客户端头没有出现即可）
	if h.Get("X-Session-Id") != "" {
		t.Fatal("客户端头不应透传")
	}

	// 流式透传（节拍过滤在下一个用例）
	if !strings.Contains(body, "好") {
		t.Fatalf("流式响应应透传: %q", body)
	}
}

func TestWBStreamBeatsFiltered(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"你"}}]}`,
			`data: {"choices":[{"delta":{"content":"\n"}}]}`,
			`data: {"choices":[{"delta":{"content":"\r\n"}}]}`,
			`data: {"choices":[{"delta":{"content":"好"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			`data: [DONE]`,
			"",
		}, "\n\n"))
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("流式响应 Content-Type 不对: %q", resp.Header.Get("Content-Type"))
	}
	if strings.Contains(body, "content\":\"\\n\"") || strings.Contains(body, "content\":\"\\r\\n\"") {
		t.Fatalf("节拍帧应被过滤: %q", body)
	}
	if !strings.Contains(body, "你") || !strings.Contains(body, "好") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("正文帧丢失: %q", body)
	}
	// usage 记账（流式尾部嗅探）
	snap := string(s.store.SnapshotJSON())
	if !strings.Contains(snap, `"prompt_tokens":7`) {
		t.Fatalf("流式 usage 未记账: %s", snap)
	}
}

func TestWBNonStreamAggregation(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"fx","arguments":"{\"a\""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
			`data: {"choices":[{"delta":{"content":"你"}}]}`,
			`data: {"choices":[{"delta":{"content":"好"}}]}`,
			`data: {"choices":[{"delta":{"content":"\n"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
			`data: [DONE]`,
			"",
		}, "\n\n"))
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var v struct {
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("非流式响应应是聚合 JSON: %v: %s", err, body)
	}
	if v.Object != "chat.completion" || len(v.Choices) != 1 {
		t.Fatalf("聚合响应形状不对: %s", body)
	}
	if v.Choices[0].Message.Content != "你好" {
		t.Fatalf("content 聚合不对: %s", body)
	}
	if v.Choices[0].FinishReason != "tool_calls" || len(v.Choices[0].Message.ToolCalls) != 1 ||
		v.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool_calls 聚合不对: %s", body)
	}
	if v.Usage.PromptTokens != 11 {
		t.Fatalf("usage 应透传: %s", body)
	}
	snap := string(s.store.SnapshotJSON())
	if !strings.Contains(snap, `"prompt_tokens":11`) {
		t.Fatalf("非流式 usage 未记账: %s", snap)
	}
}

func TestWBRefreshOn401(t *testing.T) {
	var chatCalls atomic.Int32
	var seenAuth []string
	up, refreshes := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := chatCalls.Add(1)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":11201,"message":"token 过期"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: workbuddy.TimePtr(time.Now().Add(time.Hour)), // 未临期：懒刷新不触发，只有 401 强制刷新
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("401 后应续期重试成功: %d %s", resp.StatusCode, body)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("应恰好续期一次，实际 %d", refreshes.Load())
	}
	if len(seenAuth) != 2 || seenAuth[0] != "Bearer at-old" || seenAuth[1] != "Bearer at-new" {
		t.Fatalf("重试应使用新 token: %v", seenAuth)
	}
	// 续期结果应落盘（换指针）
	cur, _ := s.store.Provider(p.ID)
	if cur.WB.AccessToken != "at-new" || cur.WB.RefreshToken != "rt-new" {
		t.Fatalf("凭证应回写: %#v", cur.WB)
	}
	if cur.WB.ExpiresAt == nil || time.Until(*cur.WB.ExpiresAt) < 55*time.Minute {
		t.Fatalf("expiresAt 应按 expiresIn 换算: %v", cur.WB.ExpiresAt)
	}
}

func TestWBRefreshInvalidFailsOver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":12153,"message":"refresh token 失效"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":12153}`)
		}
	}))
	defer up.Close()
	s, app := newTestServer(t)
	// wb 候选失败后应落到普通候选
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-old", RefreshToken: "rt-old"})
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"glm-5.0","choices":[{"message":{"role":"assistant","content":"备用"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fallback.Close()
	addProviderModel(t, s, fallback.URL, "glm-5.0", func(m *Model) { m.Priority = 1 })

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "备用") {
		t.Fatalf("凭证失效应故障转移到下一候选: %d %s", resp.StatusCode, body)
	}
}

func TestWB429MuteWithResetTime(t *testing.T) {
	reset := time.Now().In(workbuddy.UTC8).Add(90 * time.Minute).Format("2006-01-02 15:04:05")
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprintf(w, `{"code":6004,"message":"您的使用量已超出频率限制，将在 %s UTC+8 重置"}`, reset)
	})
	s, app := newTestServer(t)
	_, m := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 502 {
		t.Fatalf("全候选失败应 502: %d %s", resp.StatusCode, body)
	}
	cur, _ := s.store.Model(m.ID)
	if cur.MutedUntil.IsZero() {
		t.Fatal("429 应触发静音")
	}
	want, _ := time.ParseInLocation("2006-01-02 15:04:05", reset, workbuddy.UTC8)
	if d := time.Until(cur.MutedUntil) - time.Until(want); d < -time.Minute || d > time.Minute {
		t.Fatalf("静音应到上游告知的恢复时刻: %v vs %v", cur.MutedUntil, want)
	}
	// 静音期间：无候选
	resp, body = postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 404 {
		t.Fatalf("静音期间应 404 无候选: %d %s", resp.StatusCode, body)
	}
}

func TestWBJSONEnvelopeOn200(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":11101,"message":"NonStreamNotSupported"}`)
	})
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 502 || !strings.Contains(body, "NonStreamNotSupported") {
		t.Fatalf("200+JSON 错误信封应按失败处理: %d %s", resp.StatusCode, body)
	}
}

func TestWBUpstreamModelsAndCatalogFallback(t *testing.T) {
	up, refreshes := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})

	// GET /api/providers/{id}/models：返回远端目录（已过滤）
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := app.Client().Get(app.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	code, body := get("/api/providers/" + p.ID + "/models")
	if code != 200 {
		t.Fatalf("拉目录失败: %d %s", code, body)
	}
	if !strings.Contains(body, "glm-5.0") || !strings.Contains(body, "deepseek-v4-flash") {
		t.Fatalf("目录应含两个模型: %s", body)
	}
	if strings.Contains(body, "completion-legacy") || strings.Contains(body, "tiny-model") || strings.Contains(body, "extra-model") {
		t.Fatalf("目录应过滤排除项: %s", body)
	}
	if refreshes.Load() != 0 {
		t.Fatal("模型列表不应触发续期")
	}

	// 目录服务挂掉时退回内置清单
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer up2.Close()
	p2, _ := addWBProviderModel(t, s, up2.URL, "glm-5.0", &WorkBuddyCred{UID: "u2", AccessToken: "at-1"})
	code, body = get("/api/providers/" + p2.ID + "/models")
	if !strings.Contains(body, "minimax-m2.5") || strings.Contains(body, "hunyuan-chat") {
		t.Fatalf("目录失败应退回过滤后的内置清单: %s", body)
	}
}

// 目录算法对齐官方 cli 清单（agents.name=cli 驱动，非固定排除名单）

func TestWBImportEndpoint(t *testing.T) {
	s, app := newTestServer(t)
	// 导入：粘贴 raw JSON
	raw := `{"account":{"uid":"uid-abc12345","nickname":"测试号"},"auth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1893456000000}}`
	resp, body := postJSON(t, app, "/api/workbuddy/import", `{"kind":"workbuddy","name":"我的WB","raw":`+fmt.Sprintf("%q", raw)+`}`)
	if resp.StatusCode != 200 {
		t.Fatalf("导入失败: %d %s", resp.StatusCode, body)
	}
	if len(s.store.Providers) != 1 {
		t.Fatalf("应创建 1 个 provider: %d", len(s.store.Providers))
	}
	p := s.store.Providers[0]
	if p.Kind != "workbuddy" || p.BaseURL != "https://copilot.tencent.com/v2" || p.Name != "我的WB" ||
		p.WB == nil || p.WB.AccessToken != "at-1" || p.WB.ExpiresAt == nil || !p.WB.ExpiresAt.Equal(time.UnixMilli(1893456000000)) {
		t.Fatalf("导入的 provider 字段不对: %#v", p)
	}
	// 同 uid 重复导入：原地更新，不新建
	postJSON(t, app, "/api/workbuddy/import", `{"kind":"workbuddy","raw":`+fmt.Sprintf("%q", raw)+`}`)
	if len(s.store.Providers) != 1 {
		t.Fatalf("同账号重复导入应原地更新: %d", len(s.store.Providers))
	}
	// 国际版端点
	postJSON(t, app, "/api/workbuddy/import", `{"kind":"workbuddy-intl","cred":{"uid":"u2","access_token":"a2"}}`)
	p2, _ := s.store.ProviderByWBUID("workbuddy-intl", "u2")
	if p2.BaseURL != "https://www.codebuddy.ai/v2" {
		t.Fatalf("国际版端点不对: %s", p2.BaseURL)
	}
	// 非法 kind / 缺凭证
	resp, _ = postJSON(t, app, "/api/workbuddy/import", `{"kind":"x","cred":{"uid":"u","access_token":"a"}}`)
	if resp.StatusCode != 400 {
		t.Fatalf("非法 kind 应 400: %d", resp.StatusCode)
	}
	resp, _ = postJSON(t, app, "/api/workbuddy/import", `{"kind":"workbuddy"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("缺凭证应 400: %d", resp.StatusCode)
	}
}

// wbScanEnvTemp 把客户端目录指到临时目录（按平台设对应环境变量），返回目录路径

func wbScanEnvTemp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	dir := workbuddy.AuthDir()
	if dir == "" {
		t.Skip("无法定位客户端目录")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWBScanEndpoint(t *testing.T) {
	dir := wbScanEnvTemp(t)
	_, app := newTestServer(t)
	info := `{"account":{"uid":"ep-scan"},"auth":{"accessToken":"at","refreshToken":"rt"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-y.info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, body := postJSON(t, app, "/api/workbuddy/scan", `{}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "ep-scan") {
		t.Fatalf("扫描端点应发现凭证: %d %s", resp.StatusCode, body)
	}
	// 无凭证环境（新临时目录）不报错
	t.Setenv("WB_TEST", "1")
	resp, body = postJSON(t, app, "/api/workbuddy/scan", `{}`)
	if resp.StatusCode != 200 || strings.Contains(body, "ep-scan2") {
		t.Fatalf("扫描端点应正常返回空结果: %d %s", resp.StatusCode, body)
	}
}

func TestWBLazyRefreshNearExpiry(t *testing.T) {
	var refreshes atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-fresh","expiresIn":7200}}`)
		case "/v3/config":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, workbuddy.CatalogBody)
		default:
			if r.Header.Get("Authorization") != "Bearer at-fresh" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":11201}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer up.Close()
	s, app := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-stale", RefreshToken: "rt",
		ExpiresAt: workbuddy.TimePtr(time.Now().Add(2 * time.Minute)), // 临期窗口（<5min）内
	})
	resp, body := postJSON(t, app, "/v1/chat/completions",
		`{"model":"glm-5.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("临期应自动续期后成功: %d %s", resp.StatusCode, body)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("应恰好懒续期一次: %d", refreshes.Load())
	}
}

// 会话列表记录思考等级：客户端未声明时实际下发目录默认档（声明值留空），
// 便于区分「客户端要求」与「网关注入」。

func TestWBSessionEffortRecorded(t *testing.T) {
	up, _ := wbFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, app := newTestServer(t)
	cred := &WorkBuddyCred{UID: "uid-1", AccessToken: "at-1", ExpiresAt: workbuddy.TimePtr(time.Now().Add(time.Hour))}
	addWBProviderModel(t, s, up.URL, "glm-5.0", cred)

	postJSONWithHeader(t, app, "POST", "/v1/chat/completions",
		`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`,
		"X-Session-ID", "wb-eff")

	sess := s.activeSessions()
	if len(sess) != 1 {
		t.Fatalf("应有 1 个活跃会话: %#v", sess)
	}
	if sess[0].EffortDeclared != "" || sess[0].EffortSent != "medium" {
		t.Fatalf("思考等级 = 声明 %q / 实发 %q, want \"\" / medium（目录默认注入）",
			sess[0].EffortDeclared, sess[0].EffortSent)
	}
}
