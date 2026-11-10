package main

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
	ct := gcm.Seal(nil, nonce, []byte(plaintext), wbAAD(keyID, 1))
	env := fmt.Sprintf(`{"suite":1,"keyId":%q,"nonce":%q,"authTag":%q,"ciphertext":%q}`,
		keyID,
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		base64.StdEncoding.EncodeToString(ct[:len(ct)-16]),
	)
	return map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString([]byte(env))}
}

// ── 纯函数 ────────────────────────────────────────────────────────────────

func TestWBIDShapes(t *testing.T) {
	mid := wbMessageID()
	if len(mid) != 32 || strings.ToLower(mid) != mid {
		t.Fatalf("message id 应为 32 位小写 hex: %q", mid)
	}
	conv := wbConversationID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(conv) {
		t.Fatalf("conversation id 形状不对: %q", conv)
	}
	if len(wbSpanID()) != 16 {
		t.Fatalf("span id 应为 16 hex: %q", wbSpanID())
	}
}

func TestWBNormalizeOutbound(t *testing.T) {
	mk := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// developer → system；image_url 字符串包对象；max_completion_tokens 翻译
	body := mk(`{"messages":[
		{"role":"developer","content":"d1"},
		{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":"https://x/y.png"}]}
	],"max_completion_tokens":1234}`)
	wbNormalizeRoles(body)
	wbNormalizeImageURL(body)
	wbTranslateMaxTokens(body)
	wbEnsureStreamOptions(body)
	msgs := body["messages"].([]any)
	if r := msgs[0].(map[string]any)["role"]; r != "system" {
		t.Fatalf("developer 应折成 system，得到 %v", r)
	}
	part := msgs[1].(map[string]any)["content"].([]any)[1].(map[string]any)
	iu, ok := part["image_url"].(map[string]any)
	if !ok || iu["url"] != "https://x/y.png" {
		t.Fatalf("image_url 应包成对象: %#v", part["image_url"])
	}
	if mt, ok := body["max_tokens"].(int64); !ok || mt != 1234 {
		t.Fatalf("max_completion_tokens 应翻译成 max_tokens: %#v", body["max_tokens"])
	}
	if _, exists := body["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens 应删除")
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("应注入 stream_options.include_usage: %#v", body["stream_options"])
	}

	// tool_choice 各种形态
	cases := []struct {
		in   string
		want any // nil = 字段应被删除
	}{
		{`{"tool_choice":"auto"}`, "auto"},
		{`{"tool_choice":"NONE"}`, nil},
		{`{"tool_choice":{"type":"none"},"tools":[1],"functions":[2]}`, nil},
		{`{"tool_choice":{"type":"required"}}`, "required"},
		{`{"tool_choice":{"type":"function","function":{"name":"fx"}}}`, "fx"},
		{`{"tool_choice":{"type":"function","name":"top"}}`, "top"},
		{`{"tool_choice":{"type":"function"}}`, "auto"},
		{`{"tool_choice":{"type":"whatever"}}`, nil},
		{`{"tool_choice":3}`, nil},
	}
	for _, c := range cases {
		m := mk(c.in)
		wbNormalizeToolChoice(m)
		if c.want == nil {
			if _, exists := m["tool_choice"]; exists {
				t.Fatalf("%s: tool_choice 应删除，得到 %#v", c.in, m["tool_choice"])
			}
		} else if m["tool_choice"] != c.want {
			t.Fatalf("%s: 期望 %v，得到 %#v", c.in, c.want, m["tool_choice"])
		}
		if c.in == `{"tool_choice":{"type":"none"},"tools":[1],"functions":[2]}` {
			if _, ok := m["tools"]; ok {
				t.Fatal("none 形态应连 tools/functions 一起删")
			}
		}
	}

	// max_completion_tokens：max_tokens 已存在时只删别名；值非正整数时也只删
	m := mk(`{"max_tokens":9,"max_completion_tokens":777}`)
	wbTranslateMaxTokens(m)
	if m["max_tokens"] != float64(9) {
		t.Fatalf("已有 max_tokens 不应被覆盖: %#v", m["max_tokens"])
	}
	m = mk(`{"max_completion_tokens":0}`)
	wbTranslateMaxTokens(m)
	if _, exists := m["max_tokens"]; exists {
		t.Fatal("非正整数的别名应静默丢弃")
	}

	// 缓存键：已有则保留；没有则注入 wb2a-{uid8}-…
	m = mk(`{"prompt_cache_key":"keep-me"}`)
	wbInjectCacheKey(m, "uid-123456789")
	if m["prompt_cache_key"] != "keep-me" {
		t.Fatalf("客户端缓存键应保留: %#v", m["prompt_cache_key"])
	}
	m = mk(`{"conversation_id":"conv-9","messages":[]}`)
	wbInjectCacheKey(m, "uid-123456789")
	k, _ := m["prompt_cache_key"].(string)
	if !strings.HasPrefix(k, "wb2a-uid-1234-") || len(k) != len("wb2a-uid-1234-")+32 {
		t.Fatalf("缓存键形状不对: %q", k)
	}
	// conv 不同则键不同（会话隔离）
	m2 := mk(`{"conversation_id":"conv-10","messages":[]}`)
	wbInjectCacheKey(m2, "uid-123456789")
	if m2["prompt_cache_key"] == m["prompt_cache_key"] {
		t.Fatal("不同会话应得到不同缓存键")
	}
}

func TestWBEnsureLeadingSystem(t *testing.T) {
	inject := func(s string) []any {
		var m map[string]any
		json.Unmarshal([]byte(s), &m)
		wbEnsureLeadingSystem(m)
		return m["messages"].([]any)
	}
	msgs := inject(`{"messages":[{"role":"user","content":"hi"}]}`)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != wbDefaultSystemPrompt {
		t.Fatalf("user 打头应注入兜底 system: %#v", msgs[0])
	}
	msgs = inject(`{"messages":[{"role":"System","content":"自定义"}]}`)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "自定义" {
		t.Fatalf("system 打头（忽略大小写）不应再注入: %#v", msgs)
	}
	msgs = inject(`{"messages":[]}`)
	if len(msgs) != 0 {
		t.Fatal("空消息不应注入")
	}
}

func TestWBResolveEffort(t *testing.T) {
	meta := &wbModelMeta{Levels: []string{"low", "medium", "high"}, Default: "medium"}
	cases := []struct{ req, want string }{
		{"HIGH", "high"},   // 精确匹配（忽略大小写）
		{"minimal", "low"}, // 没有不超过其 rank 的档位时取最低档
		{"xhigh", "high"},  // 不超过请求 rank 的最大档
		{"medium", "medium"},
	}
	for _, c := range cases {
		if got := wbResolveEffort(c.req, meta); got != c.want {
			t.Fatalf("resolve(%q) = %q, want %q", c.req, got, c.want)
		}
	}
	fixed := &wbModelMeta{Fixed: "high"}
	if got := wbResolveEffort("low", fixed); got != "high" {
		t.Fatalf("单档位模型应固定: %q", got)
	}
	if got := wbResolveEffort("high", nil); got != "" {
		t.Fatalf("无目录信息应返回空: %q", got)
	}
}

func TestWBReasoningNormalize(t *testing.T) {
	mk := func(s string) map[string]any {
		var m map[string]any
		json.Unmarshal([]byte(s), &m)
		return m
	}
	meta := &wbModelMeta{Levels: []string{"low", "medium", "high"}, Default: "medium"}

	// 客户端给档位 → 解析到档位表，别名清掉
	m := mk(`{"model":"glm-5.0","reasoningEffort":"HIGH","messages":[]}`)
	if sent := wbReasoningNormalize(m, meta, "glm-5.0"); sent != "high" {
		t.Fatalf("回传实发档位 = %q, want high", sent)
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("客户端档位应解析写入: %#v", m["reasoning_effort"])
	}
	if _, ok := m["reasoningEffort"]; ok {
		t.Fatal("别名 reasoningEffort 应清除")
	}

	// 客户端 off → 原样透传，不注入其他档位字段
	m = mk(`{"model":"glm-5.0","reasoning_effort":"off","messages":[]}`)
	if sent := wbReasoningNormalize(m, meta, "glm-5.0"); sent != "off" {
		t.Fatalf("回传实发档位 = %q, want off", sent)
	}
	if m["reasoning_effort"] != "off" {
		t.Fatalf("off 应原样透传: %#v", m["reasoning_effort"])
	}
	if _, ok := m["reasoning_summary"]; ok {
		t.Fatal("off 时不应注入 reasoning_summary")
	}

	// 未给档位 → 注入目录默认
	m = mk(`{"model":"glm-5.0","messages":[]}`)
	if sent := wbReasoningNormalize(m, meta, "glm-5.0"); sent != "medium" {
		t.Fatalf("回传实发档位 = %q, want medium（目录默认）", sent)
	}
	if m["reasoning_effort"] != "medium" {
		t.Fatalf("应注入目录默认档: %#v", m["reasoning_effort"])
	}
	if m["reasoning_summary"] != "auto" {
		t.Fatalf("启用思考应补 reasoning_summary: %#v", m["reasoning_summary"])
	}

	// thinking.type=disabled → 档位字段全清
	m = mk(`{"model":"glm-5.0","reasoning_effort":"high","reasoning_summary":"auto","thinking":{"type":"disabled"},"messages":[]}`)
	if sent := wbReasoningNormalize(m, meta, "glm-5.0"); sent != "off" {
		t.Fatalf("关闭思考时回传 %q, want off", sent)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("disabled 应清除 reasoning_effort")
	}
	if _, ok := m["reasoning_summary"]; ok {
		t.Fatal("disabled 应清除 reasoning_summary")
	}

	// deepseek 启用思考：thinking.type=enabled + assistant 历史回填
	m = mk(`{"model":"deepseek-v4-flash","reasoning_effort":"high","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"a1","reasoning_content":"旧思考"},
		{"role":"assistant","content":"a2"},
		{"role":"assistant","content":"","reasoning":"纯reasoning字段"}
	]}`)
	wbReasoningNormalize(m, meta, "deepseek-v4-flash")
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Fatalf("deepseek 启用思考应补 thinking.type=enabled: %#v", m["thinking"])
	}
	msgs := m["messages"].([]any)
	a1 := msgs[1].(map[string]any)
	if a1["reasoning"] != "旧思考" {
		t.Fatalf("非空 reasoning_content 应回填 reasoning: %#v", a1)
	}
	a2 := msgs[2].(map[string]any)
	if a2["reasoning"] != " " {
		t.Fatalf("空痕迹应补占位空格: %#v", a2["reasoning"])
	}
	a3 := msgs[3].(map[string]any)
	if a3["reasoning_content"] != "纯reasoning字段" {
		t.Fatalf("只有 reasoning 字段时应拷到 reasoning_content: %#v", a3)
	}

	// 非 deepseek 且全部关闭（off 透传）→ 不回填历史
	m = mk(`{"model":"glm-5.0","reasoning_effort":"off","messages":[{"role":"assistant","content":"a"}]}`)
	wbReasoningNormalize(m, meta, "glm-5.0")
	msgs = m["messages"].([]any)
	if _, ok := msgs[0].(map[string]any)["reasoning"]; ok {
		t.Fatal("off 透传时不应回填")
	}

	// deepseek 无目录默认 → 官方 CLI 行为兜底 high + thinking.type=enabled
	m = mk(`{"model":"deepseek-v4-pro","messages":[]}`)
	if sent := wbReasoningNormalize(m, nil, "deepseek-v4-pro"); sent != "high" {
		t.Fatalf("回传实发档位 = %q, want high（deepseek 兜底）", sent)
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("deepseek 无目录默认应兜底 high: %#v", m["reasoning_effort"])
	}
	if th, ok := m["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Fatalf("deepseek 兜底应补 thinking.type=enabled: %#v", m["thinking"])
	}
	// 非 deepseek 无目录默认 → 不注入档位
	m = mk(`{"model":"glm-5.0","messages":[]}`)
	if sent := wbReasoningNormalize(m, nil, "glm-5.0"); sent != "" {
		t.Fatalf("无档位信息时应回传空，得到 %q", sent)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("非 deepseek 不应兜底档位: %#v", m["reasoning_effort"])
	}
	// deepseek 显式 off → 兜底不得覆盖
	m = mk(`{"model":"deepseek-v4-pro","reasoning_effort":"off","messages":[]}`)
	wbReasoningNormalize(m, nil, "deepseek-v4-pro")
	if m["reasoning_effort"] != "off" {
		t.Fatalf("显式 off 不应被兜底覆盖: %#v", m["reasoning_effort"])
	}
}

func TestWBClassify(t *testing.T) {
	reset := time.Now().In(wbUTC8).Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	cases := []struct {
		name   string
		status int
		body   string
		class  wbErrClass
	}{
		{"401", 401, `{}`, wbErrTokenExpired},
		{"12153", 200, `{"code":12153,"message":"refresh token 失效"}`, wbErrTokenExpired},
		{"11128", 200, `{"code":11128,"message":"Illegal API invocation"}`, wbErrContentBlocked},
		{"429 文本", 429, `{"message":"Too Many Requests"}`, wbErrQuota},
		{"6004 带恢复时刻", 429, `{"code":6004,"message":"您的使用量已超出频率限制，将在 ` + reset + ` UTC+8 重置"}`, wbErrQuota},
		{"402", 402, `{}`, wbErrQuota},
		{"14001 额度耗尽", 200, `{"code":14001}`, wbErrQuota},
		{"限流文案", 200, `{"message":"请求过于频繁"}`, wbErrQuota},
		{"11101 请求形态", 400, `{"code":11101}`, wbErrFatal},
		{"账号受限", 200, `{"code":11140,"message":"account has been restricted"}`, wbErrFatal},
		{"其他 fatal", 500, `{"message":"boom"}`, wbErrFatal},
	}
	for _, c := range cases {
		class, code, msg, resetAt := wbClassify(c.status, []byte(c.body))
		if class != c.class {
			t.Fatalf("%s: 分类 %d ≠ %d（msg=%s）", c.name, class, c.class, msg)
		}
		if c.name == "6004 带恢复时刻" {
			if resetAt.IsZero() {
				t.Fatalf("应解析出恢复时刻: %q", msg)
			}
			want, _ := time.ParseInLocation("2006-01-02 15:04:05", reset, wbUTC8)
			if d := time.Until(resetAt) - time.Until(want); d < -time.Minute || d > time.Minute {
				t.Fatalf("恢复时刻偏差过大: %v vs %v", resetAt, want)
			}
		}
		_ = code
	}
	// 业务码在 data/error/response.data 嵌套里也要找到
	_, code, msg, _ := wbClassify(200, []byte(`{"data":{"code":6004},"error":{"message":"x"}}`))
	if code != 6004 || msg != "x" {
		t.Fatalf("嵌套业务码/消息提取不对: code=%d msg=%q", code, msg)
	}
}

func TestWBParseResetAt(t *testing.T) {
	t1 := wbParseResetAt("将于 2026-09-11 19:43:46 UTC+8 重置")
	if t1.IsZero() {
		t.Fatal("应解析出绝对恢复时刻")
	}
	want := time.Date(2026, 9, 11, 19, 43, 46, 0, wbUTC8)
	if !t1.Equal(want) {
		t.Fatalf("解析结果 %v ≠ %v", t1, want)
	}
	if !wbParseResetAt("无效日期 2026-02-31 10:00:00 UTC+8").IsZero() {
		t.Fatal("溢出日期应判无效")
	}
	if !wbParseResetAt("没有任何时间信息").IsZero() {
		t.Fatal("无信息应返回零值")
	}
	// 当日 HH:MM：已过时刻视为没有信息，未来时刻返回今天。
	// 临近午夜时 now+70min 会跨天，HH:MM 无法与「今天」区分，跳过该场景避免 flaky
	future := time.Now().In(wbUTC8).Add(time.Hour + 10*time.Minute)
	if future.Day() != time.Now().In(wbUTC8).Day() {
		t.Skip("临近午夜，当日 HH:MM 场景跨天不可测")
	}
	t2 := wbParseResetAt("当日 " + future.Format("15:04") + " 恢复")
	if t2.IsZero() {
		t.Fatal("当日未来时刻应解析")
	}
	if t2.Hour() != future.Hour() || t2.Minute() != future.Minute() {
		t.Fatalf("当日时刻 %v ≠ %v", t2, future)
	}
}

func TestWBSSEFilter(t *testing.T) {
	beat := "data: {\"choices\":[{\"delta\":{\"content\":\"\\n\"}}]}\n\n"
	beat2 := "data: {\"choices\":[{\"delta\":{\"content\":\"\\r\\n\"}}]}\n\n"
	real := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"
	usage := "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":1}}\n\n"
	done := "data: [DONE]\n\n"
	trailing := "data: {\"choices\":[{\"delta\":{\"content\":\"尾\"}}]}\n" // 流尾无空行结束
	in := beat + real + beat2 + usage + done + trailing

	f := &wbSSEFilter{r: strings.NewReader(in)}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "content\":\"\\n\"") || strings.Contains(got, "content\":\"\\r\\n\"") {
		t.Fatalf("节拍帧应被过滤: %q", got)
	}
	for _, want := range []string{real, usage, done, trailing} {
		if !strings.Contains(got, want) {
			t.Fatalf("正常帧丢失 %q: %q", want, got)
		}
	}
	// 真正含换行的正文（换行混在其他字符里）不丢
	mixed := "data: {\"choices\":[{\"delta\":{\"content\":\"a\\nb\"}}]}\n\n"
	f2 := &wbSSEFilter{r: strings.NewReader(mixed)}
	out2, _ := io.ReadAll(f2)
	if string(out2) != mixed {
		t.Fatalf("混合内容帧不应被过滤: %q", out2)
	}
}

func TestWBSSEFilterReasoningMerge(t *testing.T) {
	// 上游形态：一个分片一个 token，夹纯空格节拍，正文/收尾/用量帧混在中间
	rc := func(s string) string {
		b, _ := json.Marshal(s)
		return `data: {"choices":[{"delta":{"content":"","reasoning_content":` + string(b) +
			`,"refusal":""},"finish_reason":null}],"id":"t","usage":null}` + "\n\n"
	}
	content := "data: {\"choices\":[{\"delta\":{\"content\":\"答\"}}]}\n\n"
	usage := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	done := "data: [DONE]\n\n"
	heartbeat := ": heartbeat\n\n" // 思考中途的上游心跳：透传且不打断持有
	in := rc("Let") + rc(" me") + rc(" ") + rc(" ") + heartbeat + rc("think") + rc(".") + rc("\n\n") +
		content + rc("Next") + rc(" line") + rc(".") + usage + done + rc("尾") // 末帧无空行 → EOF 冲刷

	f := &wbSSEFilter{r: strings.NewReader(in)}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// 正文/用量/[DONE]/心跳 原样放行
	for _, want := range []string{content, usage, done, heartbeat} {
		if !strings.Contains(got, want) {
			t.Fatalf("正常帧丢失 %q: %q", want, got)
		}
	}
	// 解析放行后的 reasoning 增量
	var deltas []string
	for _, frame := range strings.Split(got, "\n\n") {
		if d, ok := strings.CutPrefix(frame, "data: "); ok {
			var v struct {
				Choices []struct {
					Delta struct {
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(d), &v) == nil && len(v.Choices) > 0 &&
				v.Choices[0].Delta.ReasoningContent != "" {
				deltas = append(deltas, v.Choices[0].Delta.ReasoningContent)
			}
		}
	}
	joined := strings.Join(deltas, "")
	if joined != "Let me  think.\n\nNext line.尾" {
		t.Fatalf("合并后思考文本不完整: %q", joined)
	}
	// 心跳不打断持有：仍按思考段一次放行（content 前一段、usage 前一段、EOF 一段）
	if len(deltas) != 3 {
		t.Fatalf("应按思考段一次放行: %d %q", len(deltas), deltas)
	}
	// 逐 token 原文不应出现（合成分片里不会是单个 token）
	if strings.Contains(got, `"reasoning_content":"Let"`) {
		t.Fatalf("逐 token 分片未被合并: %q", got)
	}
}

func TestWBSSEFilterReasoningKeepalive(t *testing.T) {
	// 长思考持有期间发 SSE 注释行保活：不产生 data 帧，思考仍整段一次放行
	old := wbKeepaliveEvery
	wbKeepaliveEvery = -time.Second // 覆写为负值使条件恒真验证该路径
	defer func() { wbKeepaliveEvery = old }()

	rc := func(s string) string {
		b, _ := json.Marshal(s)
		return `data: {"choices":[{"delta":{"reasoning_content":` + string(b) + `}}]}` + "\n\n"
	}
	in := rc("一") + rc("二") + rc("三") + "data: [DONE]\n\n"
	f := &wbSSEFilter{r: strings.NewReader(in)}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if n := strings.Count(got, ": keepalive"); n != 3 {
		t.Fatalf("每个持有分片应触发一次保活注释: %d %q", n, got)
	}
	if n := strings.Count(got, "reasoning_content"); n != 1 {
		t.Fatalf("思考应整段一次放行: %d %q", n, got)
	}
	if !strings.Contains(got, "一二三") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("内容或 [DONE] 丢失: %q", got)
	}
}

func TestWBAgg(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"你"}}]}`,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"思考"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"fx","arguments":"{\"a\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	agg := &wbAgg{}
	if _, err := agg.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	body, usage := agg.result("glm-5.0")
	var v struct {
		Model   string `json:"model"`
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("聚合响应不是合法 JSON: %v: %s", err, body)
	}
	if v.Model != "glm-5.0" || v.Object != "chat.completion" || len(v.Choices) != 1 {
		t.Fatalf("基本字段不对: %s", body)
	}
	m := v.Choices[0].Message
	if m.Content != "你好" || m.ReasoningContent != "思考" || m.Role != "assistant" {
		t.Fatalf("消息聚合不对: %#v", m)
	}
	if v.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 应取最后非空值: %q", v.Choices[0].FinishReason)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" ||
		m.ToolCalls[0].Function.Name != "fx" || m.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool_calls 聚合不对: %#v", m.ToolCalls)
	}
	if usage != (Usage{Prompt: 7, Completion: 3, Cached: 2}) {
		t.Fatalf("usage 解析不对: %#v", usage)
	}
}

func TestWBBuiltinCatalog(t *testing.T) {
	cat := wbBuiltinCatalog()
	for _, id := range cat.IDs {
		for _, p := range wbExcludedPrefixes {
			if strings.HasPrefix(id, p) {
				t.Fatalf("内置清单应过滤掉 %q（命中前缀 %q）", id, p)
			}
		}
	}
	for _, want := range []string{"auto", "glm-5.0", "deepseek-v4-flash", "hunyuan-2.0-instruct"} {
		if _, ok := cat.Meta[want]; !ok {
			t.Fatalf("内置清单缺少 %q", want)
		}
	}
	// 准入规则
	if wbAdmitModel("x", map[string]any{"maxOutputTokens": float64(48000)}) != true {
		t.Fatal("正常模型应准入")
	}
	if wbAdmitModel("x", map[string]any{}) != false {
		t.Fatal("缺 maxOutputTokens 应拒绝")
	}
	if wbAdmitModel("x", map[string]any{"maxOutputTokens": float64(8192)}) != false {
		t.Fatal("maxOutputTokens < 16000 应拒绝")
	}
	if wbAdmitModel("x", map[string]any{"maxOutputTokens": float64(48000), "supportsExtra": true}) != false {
		t.Fatal("supportsExtra 真值应拒绝")
	}
	if wbAdmitModel("x", map[string]any{"maxOutputTokens": float64(48000), "disabled": true}) != false {
		t.Fatal("disabled 应拒绝")
	}
}

func TestWBParseInfoBytes(t *testing.T) {
	// 标准嵌套形态
	hit := wbParseInfoBytes([]byte(`{
		"account":{"uid":"uid-123456789","nickname":"小明","enterpriseId":"ent-1","departmentFullName":"研发"},
		"auth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1893456000000,"domain":"x"}
	}`), "")
	if hit == nil || hit.Cred == nil {
		t.Fatal("应解析出凭证")
	}
	c := hit.Cred
	if c.UID != "uid-123456789" || c.AccessToken != "at-1" || c.RefreshToken != "rt-1" ||
		c.EnterpriseID != "ent-1" || c.Department != "研发" || c.Domain != "x" {
		t.Fatalf("字段提取不对: %#v", c)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(time.UnixMilli(1893456000000)) {
		t.Fatalf("expiresAt 应按毫秒换算: %v", c.ExpiresAt)
	}
	// 扁平形态
	hit = wbParseInfoBytes([]byte(`{"uid":"u2","accessToken":"at2","expiresAt":1}`), "")
	if hit == nil || hit.Cred.AccessToken != "at2" {
		t.Fatal("扁平形态应兼容")
	}
	// 缺 accessToken：跳过
	if wbParseInfoBytes([]byte(`{"uid":"u3"}`), "") != nil {
		t.Fatal("缺 accessToken 应返回 nil")
	}
	// 加密信封：无密钥 → Encrypted 占位；有密钥 → 解开
	secret := "test-at-rest-secret-key-44chars-padding!!"
	enc := wbSealField(t, secret, "secret-token")
	raw, _ := json.Marshal(map[string]any{"uid": "u4", "accessToken": enc})
	hit = wbParseInfoBytes(raw, "")
	if hit == nil || !hit.Encrypted || hit.Cred != nil {
		t.Fatalf("无密钥应返回加密占位: %#v", hit)
	}
	hit = wbParseInfoBytes(raw, secret)
	if hit == nil || hit.Cred == nil || hit.Cred.AccessToken != "secret-token" {
		t.Fatalf("正确密钥应解开信封: %#v", hit)
	}
	// 密钥不匹配
	if hit := wbParseInfoBytes(raw, "wrong-key"); hit == nil || !hit.Encrypted {
		t.Fatal("错误密钥应保持加密占位")
	}
}

// ── 转发链路（伪造上游） ──────────────────────────────────────────────────

const wbCatalogBody = `{"code":0,"data":{
	"agents":[{"name":"desktop","models":["completion-legacy"]},{"name":"cli","models":["GLM-5.0","deepseek-v4-flash","blocked-model","ghost-model"]}],
	"availableModels":["GLM-5.0","deepseek-v4-flash","blocked-model","future-x"],
	"models":[
	{"id":"GLM-5.0","reasoning":{"supportedEfforts":["low","medium","high"],"defaultEffort":"medium"},"maxOutputTokens":48000},
	{"id":"deepseek-v4-flash","reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"},"maxOutputTokens":50000},
	{"id":"completion-legacy","maxOutputTokens":48000},
	{"id":"tiny-model","maxOutputTokens":8000},
	{"id":"extra-model","maxOutputTokens":48000,"supportsExtra":true},
	{"id":"blocked-model","maxOutputTokens":48000,"disabled":true}
]}}`

// wbFakeUpstream 起一个 WorkBuddy 形状的伪造上游；chat 决定 /v2/chat/completions 的行为
func wbFakeUpstream(t *testing.T, chat http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	refreshes := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, wbCatalogBody)
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
		ExpiresAt: wbTimePtr(time.Now().Add(time.Hour))}
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
		"User-Agent":          wbCLIUserAgent(),
		"X-IDE-Type":          "CLI",
		"X-IDE-Name":          "CLI",
		"X-IDE-Version":       wbCLIVersion,
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
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != wbDefaultSystemPrompt {
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
		ExpiresAt: wbTimePtr(time.Now().Add(time.Hour)), // 未临期：懒刷新不触发，只有 401 强制刷新
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
	reset := time.Now().In(wbUTC8).Add(90 * time.Minute).Format("2006-01-02 15:04:05")
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
	want, _ := time.ParseInLocation("2006-01-02 15:04:05", reset, wbUTC8)
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
func TestWBCatalogCLIScope(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string // 逗号分隔的目录 ID；空 = 应报错
		wantErr string // 期望的错误关键词
	}{
		{
			name: "cli 渠道清单过滤 + 去重",
			body: `{"code":0,"data":{"agents":[{"name":"other","models":["chat-model"]},{"name":"cli","models":["new-model","new-model","ghost"]}],
				"models":[{"id":"glm-4.6"},{"id":"new-model"},{"id":"chat-model"}]}}`,
			want: "new-model",
		},
		{
			name: "availableModels 非空时取交集",
			body: `{"code":0,"data":{"agents":[{"name":"cli","models":["new-model","canonical"]}],
				"availableModels":["canonical","chat-model"],
				"models":[{"id":"new-model"},{"id":"canonical"}]}}`,
			want: "canonical",
		},
		{
			name: "availableModels 为空不过滤",
			body: `{"code":0,"data":{"agents":[{"name":"cli","models":["new-model"]}],
				"availableModels":[],"models":[{"id":"new-model"}]}}`,
			want: "new-model",
		},
		{
			name:    "无 cli 渠道清单报错",
			body:    `{"code":0,"data":{"agents":[{"name":"other","models":["new-model"]}],"models":[{"id":"new-model"}]}}`,
			wantErr: "cli",
		},
		{
			name:    "缺 agents 报错",
			body:    `{"code":0,"data":{"models":[{"id":"new-model"}]}}`,
			wantErr: "cli",
		},
		{
			name: "历史嵌套形态 data.agent.agents 兼容",
			body: `{"code":0,"data":{"agent":{"agents":[{"name":"cli","models":["new-model"]}]},"models":[{"id":"new-model"}]}}`,
			want: "new-model",
		},
		{
			name: "disabled 剔除，筛后为空报错",
			body: `{"code":0,"data":{"agents":[{"name":"cli","models":["blocked"]}],
				"models":[{"id":"blocked","disabled":true}]}}`,
			wantErr: "没有可用模型",
		},
		{
			name: "清单里没有定义的模型（无服务配置）不进目录",
			body: `{"code":0,"data":{"agents":[{"name":"cli","models":["ghost"]}],
				"models":[{"id":"new-model"}]}}`,
			wantErr: "没有可用模型",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, c.body)
			}))
			defer up.Close()
			cat, err := wbFetchCatalog(&http.Client{Timeout: 5 * time.Second}, up.URL, &wbCN, &WorkBuddyCred{UID: "u", AccessToken: "a"})
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("应报错含 %q: %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got := strings.Join(cat.IDs, ","); got != c.want {
				t.Fatalf("目录 = %q, want %q", got, c.want)
			}
		})
	}
}

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
	dir := wbAuthDir()
	if dir == "" {
		t.Skip("无法定位客户端目录")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWBScanDedupeNewest(t *testing.T) {
	dir := wbScanEnvTemp(t)
	info := `{"account":{"uid":"scan-uid"},"auth":{"accessToken":"at","refreshToken":"rt"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-x.info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	// 同 uid 更旧的快照：应被更新的一份覆盖
	old := filepath.Join(dir, "Tencent-Cloud.coding-copilot-old.info")
	if err := os.WriteFile(old, []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	hits := wbScan("")
	if len(hits) != 1 {
		t.Fatalf("应按 uid 去重出 1 条: %#v", hits)
	}
	if hits[0].Cred == nil || hits[0].Cred.AccessToken != "at" || hits[0].App != "WorkBuddy 桌面端" {
		t.Fatalf("命中内容不对: %#v", hits[0])
	}
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
			fmt.Fprint(w, wbCatalogBody)
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
		ExpiresAt: wbTimePtr(time.Now().Add(2 * time.Minute)), // 临期窗口（<5min）内
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
	cred := &WorkBuddyCred{UID: "uid-1", AccessToken: "at-1", ExpiresAt: wbTimePtr(time.Now().Add(time.Hour))}
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
