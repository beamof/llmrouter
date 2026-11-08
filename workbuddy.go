package main

// ─────────────────────────── WorkBuddy 上游适配 ───────────────────────────
//
// 最初移植自 Agent2Providers（AGPL-3.0）的 workbuddy 适配器，出站归一化、推理档位、
// token 续期与错误分类沿用其行为；请求头族、模型目录算法与默认系统提示词已对齐
// 官方 CLI（@tencent-ai/codebuddy-code）契约，参照 ccLoad internal/codebuddyauth
// 的实现与 UPSTREAM.md 协议说明。三个与透传不同的要点：
//   · 上游 /v2/chat/completions 只支持流式（stream:false 返回 code 11101），出站一律
//     强制 stream:true，非流式客户端由本网关把 SSE 聚合成 chat.completion；
//   · 上游会在正文分片之间插入「整片只有换行」的节拍分片（实测约占三分之一），
//     透传时按帧过滤，聚合时同样先过滤再聚合；
//   · 对话接口要求 messages[0].role 为 system，缺失时注入官方 CLI 默认提示词。

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// wbCLIVersion 对齐官方 @tencent-ai/codebuddy-code 的版本（ccLoad 同步使用）。
// UA 与 X-IDE-Version 共用同一值，避免指纹内部漂移。
const wbCLIVersion = "2.162.0"

// wbCLIUserAgent 官方 CLI 形态的 User-Agent。上游据 UA 的 CLI 段识别 CLI 渠道：
// 目录接口不带时只返回旧版模型清单。
func wbCLIUserAgent() string { return "CLI/" + wbCLIVersion + " CodeBuddy/" + wbCLIVersion }

// wbEdition WorkBuddy/CodeBuddy 的地区身份：两端协议一致，差别只有端点与语言。
// 产品已由 WorkBuddy 更名 CodeBuddy：国内版控制面端点为 copilot.tencent.com
// （历史 www.workbuddy.cn 仍在线，存量 provider 的基址不动、继续可用），
// 国际版为 www.codebuddy.ai（历史 www.workbuddy.ai 同站，仍可服务）。
type wbEdition struct {
	id        string // "cn" / "intl"
	label     string
	endpoint  string
	webOrigin string // 目录/续期等控制面请求的 Origin/Referer（官方 web 控制台域名）
	lang      string // Accept-Language
	intl      bool
}

var (
	wbCN = wbEdition{
		id: "cn", label: "国内版", endpoint: "https://copilot.tencent.com",
		webOrigin: "https://www.codebuddy.cn", lang: "zh-CN",
	}
	wbIntl = wbEdition{
		id: "intl", label: "国际版", endpoint: "https://www.codebuddy.ai",
		webOrigin: "https://www.codebuddy.ai", lang: "en-US",
		intl: true,
	}
)

// wbOf 返回 provider 的 WorkBuddy 地区身份与凭证；非 WorkBuddy provider 返回 nil
func wbOf(p Provider) (*wbEdition, *WorkBuddyCred) {
	switch p.Kind {
	case "workbuddy":
		return &wbCN, p.WB
	case "workbuddy-intl":
		return &wbIntl, p.WB
	}
	return nil, nil
}

// wbEndpoint 从 provider 基址还原上游端点（导入时写为 {endpoint}/v2；
// 去掉 /v2 后缀后即是端点，不带该后缀时按原样使用，便于自定义/中转端点）
func wbEndpoint(p Provider) string {
	return strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/v2")
}

// ── 请求标识 ──────────────────────────────────────────────────────────────

// wbUUID7 生成 UUIDv7 原始字节：48bit 毫秒时间戳 + 版本 7 + 随机位（crypto/rand）。
// 适配器用非加密随机源；这里换 crypto/rand 更稳妥，上游不校验随机性。
func wbUUID7() [16]byte {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	rand.Read(b[6:])
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return b
}

// wbMessageID 32 位小写 hex（X-Request-ID / X-Trace-ID 等）
func wbMessageID() string {
	b := wbUUID7()
	return hex.EncodeToString(b[:])
}

// wbConversationID 连字符 8-4-4-4-12 形式（X-Conversation-ID）
func wbConversationID() string {
	b := wbUUID7()
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// wbSpanID 16 位小写 hex（traceparent / b3）
func wbSpanID() string {
	b := wbUUID7()
	return hex.EncodeToString(b[:8])
}

// ── 请求头 ────────────────────────────────────────────────────────────────

// wbAuthHeaders 鉴权与身份头（对话与框架请求共用，非空字段才带）
func wbAuthHeaders(h http.Header, cred *WorkBuddyCred) {
	if cred.UID != "" {
		h.Set("X-User-Id", cred.UID)
	}
	if cred.AccessToken != "" {
		h.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	if cred.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", cred.EnterpriseID)
		h.Set("X-Tenant-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		h.Set("X-Domain", cred.Domain)
	}
	if cred.Department != "" {
		h.Set("X-Department-Info", cred.Department)
	}
}

// wbChatHeader 构造 POST /v2/chat/completions 的完整请求头，对齐官方 CLI 的会话
// 指纹（参照 ccLoad EnsureChatFingerprint）。要点：Origin/Referer/X-Domain 跟随
// 实际请求端点，X-Domain 不取凭证 domain（凭证 domain 只用于控制面请求）。
// 不转发客户端任何头。
func wbChatHeader(e *wbEdition, cred *WorkBuddyCred, endpoint string) http.Header {
	reqID, msgID := wbMessageID(), wbMessageID()
	traceID, spanID := wbMessageID(), wbSpanID()
	origin, host := endpoint, ""
	if u, err := url.Parse(strings.TrimRight(endpoint, "/")); err == nil && u.Host != "" {
		origin, host = u.Scheme+"://"+u.Host, u.Hostname()
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream, application/json")
	h.Set("Accept-Language", e.lang)
	h.Set("Accept-Encoding", "identity") // 不让 transport 透明加 gzip，SSE 过滤按明文字节处理
	h.Set("User-Agent", wbCLIUserAgent())
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-IDE-Type", "CLI")
	h.Set("X-IDE-Name", "CLI")
	h.Set("X-IDE-Version", wbCLIVersion)
	h.Set("X-Product", "SaaS")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
	h.Set("X-Conversation-ID", wbConversationID())
	h.Set("X-Conversation-Request-ID", reqID)
	h.Set("X-Root-Request-ID", reqID)
	h.Set("X-Conversation-Message-ID", msgID)
	h.Set("X-Request-ID", msgID)
	// 追踪族（W3C traceparent / b3）：官方客户端亦携带，保持每请求随机
	h.Set("X-Trace-ID", traceID)
	h.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	h.Set("b3", traceID+"-"+spanID+"-1")
	h.Set("X-B3-TraceId", traceID)
	h.Set("X-B3-SpanId", spanID)
	h.Set("X-B3-Sampled", "1")
	wbAuthHeaders(h, cred)
	if host != "" {
		h.Set("X-Domain", host)
	}
	return h
}

// wbFrameworkHeader 续期/目录等控制面请求的头，对齐官方 CLI 的来源指纹：
// UA 带 CLI 段（上游据它识别 CLI 渠道，不带时 /v3/config 只返回旧版模型清单），
// Origin/Referer 用官方 web 控制台域名（国内版 codebuddy.cn，国际版跟随端点）。
func wbFrameworkHeader(e *wbEdition, cred *WorkBuddyCred) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", e.lang)
	h.Set("Content-Type", "application/json")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Request-ID", wbMessageID())
	h.Set("X-Product", "SaaS")
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("User-Agent", wbCLIUserAgent())
	h.Set("Origin", e.webOrigin)
	h.Set("Referer", e.webOrigin+"/")
	wbAuthHeaders(h, cred)
	return h
}

// ── 出站归一化 ────────────────────────────────────────────────────────────

// wbDefaultSystemPrompt 上游要求首条消息是 system，缺失时注入的官方 CLI 默认提示词
// （@tencent-ai/codebuddy-code 出站最终化阶段补入的原句，照抄含原始排版）
const wbDefaultSystemPrompt = "You are CodeBuddy Code.You are an interactive CLI tool that helps users with software engineering tasks."

func wbMessages(body map[string]any) []any {
	msgs, _ := body["messages"].([]any)
	return msgs
}

// wbNormalizeOutbound 出站归一化，顺序与适配器一致：roles → tool_choice → image_url
// → max_completion_tokens → stream_options → prompt_cache_key（推理档位与首条 system
// 的注入由调用方在其后完成）。
func wbNormalizeOutbound(body map[string]any, uid string) {
	wbNormalizeRoles(body)
	wbNormalizeToolChoice(body)
	wbNormalizeImageURL(body)
	wbTranslateMaxTokens(body)
	wbEnsureStreamOptions(body)
	wbInjectCacheKey(body, uid)
}

// wbNormalizeRoles 上游不认 developer 角色，统一折成 system
func wbNormalizeRoles(body map[string]any) {
	for _, m := range wbMessages(body) {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
		}
	}
}

// wbNormalizeToolChoice 上游 tool_choice 只认字符串；"none" 一律连 tools/functions 一起删
func wbNormalizeToolChoice(body map[string]any) {
	tc, ok := body["tool_choice"]
	if !ok {
		return
	}
	dropTools := func() {
		delete(body, "tool_choice")
		delete(body, "tools")
		delete(body, "functions")
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			dropTools()
		} // 其余字符串原样保留
	case map[string]any:
		t, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "none":
			dropTools()
		case "auto", "required":
			body["tool_choice"] = strings.ToLower(strings.TrimSpace(t))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				name, _ = v["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				name = "auto"
			}
			body["tool_choice"] = name
		default:
			delete(body, "tool_choice")
		}
	default: // 数字/数组/null 等上游不认的形态
		delete(body, "tool_choice")
	}
}

// wbNormalizeImageURL 上游 image_url 只认对象形态，字符串形态包一层 {"url":…}
func wbNormalizeImageURL(body map[string]any) {
	for _, m := range wbMessages(body) {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t != "image_url" {
				continue
			}
			if s, ok := part["image_url"].(string); ok && s != "" {
				part["image_url"] = map[string]any{"url": s}
			}
		}
	}
}

// wbTranslateMaxTokens 上游只认 max_tokens；别名仅在目标字段不存在且值为正整数时翻译
func wbTranslateMaxTokens(body map[string]any) {
	mc, ok := body["max_completion_tokens"]
	if !ok {
		return
	}
	delete(body, "max_completion_tokens")
	if _, exists := body["max_tokens"]; exists {
		return
	}
	f, ok := mc.(float64)
	if !ok || f <= 0 || f != math.Trunc(f) {
		return
	}
	body["max_tokens"] = int64(f)
}

// wbEnsureStreamOptions usage 只随最终 SSE 帧返回，出站必须带 include_usage。
// 客户端传 include_usage:false 时也强制改回 true：流式请求的用量记账唯一来源
// 就是最终帧里的 usage，关掉等于放弃统计（其余 stream_options 字段原样保留）。
func wbEnsureStreamOptions(body map[string]any) {
	so, _ := body["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	body["stream_options"] = so
}

// wbInjectCacheKey 注入前缀缓存键（实测 8k 上下文命中后费用差约 17 倍）。
// 键形如 wb2a-{uid前8}-{sha256(uid|conv) 前 16 字节的 hex}；conv 取请求里的会话标识，
// 客户端已带 prompt_cache_key 时原样保留。
func wbInjectCacheKey(body map[string]any, uid string) {
	if s, ok := body["prompt_cache_key"].(string); ok && strings.TrimSpace(s) != "" {
		return
	}
	conv := ""
	meta, _ := body["metadata"].(map[string]any)
	for _, v := range []any{body["conversation_id"], body["conversationId"], meta["conversation_id"], meta["conversationId"]} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			conv = strings.TrimSpace(s)
			break
		}
	}
	uid8 := "-"
	if uid != "" {
		uid8 = uid
		if len(uid) > 8 {
			uid8 = uid[:8]
		}
	}
	sum := sha256.Sum256([]byte(uid + "|" + conv))
	body["prompt_cache_key"] = "wb2a-" + uid8 + "-" + hex.EncodeToString(sum[:16])
}

// wbEnsureLeadingSystem 上游要求首条消息为 system，缺失时前插兜底提示词。
// 必须在归一化之后调用：developer 打头先折成 system 才判得出「首条已是 system」。
func wbEnsureLeadingSystem(body map[string]any) {
	msgs := wbMessages(body)
	if len(msgs) == 0 {
		return
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return
	}
	if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
		return
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": wbDefaultSystemPrompt}}, msgs...)
}

// ── 推理档位 ──────────────────────────────────────────────────────────────
// 档位由模型目录驱动（/v3/config 的 reasoning 元数据），出站只写 reasoning_effort；
// 目录拉不到时无档位信息，客户端字段原样透传。

var wbEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func wbEffortRank(s string) int {
	for i, e := range wbEfforts {
		if strings.EqualFold(e, s) {
			return i
		}
	}
	return -1
}

func wbIsOff(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "off" || s == "none"
}

// wbModelMeta 模型的推理档位元数据（来自目录条目）
type wbModelMeta struct {
	Levels   []string // supportedEfforts，去重保序
	Declared string   // reasoning.effort
	Default  string   // defaultEffort 或 declared（经档位表校验）
	Fixed    string   // 无档位表时取 declared，单档位取唯一值
	Supports *bool    // supportsReasoning，nil = 未声明
}

// wbMetaFromModel 从目录条目解析推理元数据
func wbMetaFromModel(m map[string]any) *wbModelMeta {
	meta := &wbModelMeta{}
	r, _ := m["reasoning"].(map[string]any)
	seen := map[string]bool{}
	if arr, ok := r["supportedEfforts"].([]any); ok {
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				continue
			}
			s = strings.TrimSpace(s)
			if s == "" || seen[strings.ToLower(s)] {
				continue
			}
			seen[strings.ToLower(s)] = true
			meta.Levels = append(meta.Levels, s)
		}
	}
	meta.Declared, _ = r["effort"].(string)
	meta.Default, _ = r["defaultEffort"].(string)
	if meta.Default == "" {
		meta.Default = meta.Declared
	}
	switch {
	case len(meta.Levels) == 0:
		meta.Fixed = meta.Declared
	case len(meta.Levels) == 1:
		meta.Fixed = meta.Levels[0]
	}
	// default 必须是档位表成员，否则退回 fixed
	if len(meta.Levels) > 0 && meta.Default != "" && !wbContainsFold(meta.Levels, meta.Default) {
		meta.Default = meta.Fixed
	}
	if b, ok := m["supportsReasoning"].(bool); ok {
		meta.Supports = &b
	}
	return meta
}

func wbContainsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// wbResolveEffort 把请求档位解析到目录允许的值：精确匹配 → 固定档 →
// 不超过请求 rank 的最大档（全部比请求高时取最低档）→ default。
func wbResolveEffort(requested string, m *wbModelMeta) string {
	if m == nil {
		return ""
	}
	for _, l := range m.Levels {
		if strings.EqualFold(l, requested) {
			return l
		}
	}
	if m.Fixed != "" {
		return m.Fixed
	}
	if r := wbEffortRank(requested); r >= 0 {
		best, bestRank := "", -1
		for _, l := range m.Levels {
			lr := wbEffortRank(l)
			if lr < 0 {
				continue
			}
			if lr <= r && (bestRank < 0 || lr > bestRank) {
				best, bestRank = l, lr
			}
		}
		if best != "" {
			return best
		}
		min := m.Levels[0] // 全部档位都比请求高时取最低档
		for _, l := range m.Levels {
			if wbEffortRank(l) >= 0 && wbEffortRank(l) < wbEffortRank(min) {
				min = l
			}
		}
		return min
	}
	return m.Default
}

// wbClientEffort 客户端显式给的档位（含别名 reasoningEffort / reasoning.effort）
func wbClientEffort(body map[string]any) (string, bool) {
	for _, v := range []any{body["reasoning_effort"], body["reasoningEffort"]} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), true
		}
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if s, ok := r["effort"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), true
		}
	}
	return "", false
}

func wbThinkingType(body map[string]any) string {
	t, _ := body["thinking"].(map[string]any)
	s, _ := t["type"].(string)
	return s
}

func wbIsRole(m map[string]any, role string) bool {
	r, _ := m["role"].(string)
	return strings.EqualFold(strings.TrimSpace(r), role)
}

// wbReasoningNormalize 推理档位出站处理（目录驱动）：
// 客户端已给档位 → 解析后写入 reasoning_effort 并清掉别名（off/none 原样透传）；
// 未给且未显式关闭 → 注入目录默认档（不硬编码）；
// thinking.type=disabled → 移除全部档位字段；
// deepseek 系启用思考时补 thinking.type=enabled、reasoning_summary 与历史 reasoning 回填。
func wbReasoningNormalize(body map[string]any, m *wbModelMeta, modelID string) {
	isDeepSeek := strings.HasPrefix(strings.ToLower(modelID), "deepseek")
	if strings.EqualFold(strings.TrimSpace(wbThinkingType(body)), "disabled") {
		delete(body, "reasoning_effort")
		delete(body, "reasoningEffort")
		delete(body, "reasoning_summary")
		if r, ok := body["reasoning"].(map[string]any); ok {
			delete(r, "effort")
		}
		wbBackfillReasoning(body, isDeepSeek, false)
		return
	}
	if effort, ok := wbClientEffort(body); ok {
		resolved := wbResolveEffort(effort, m)
		switch {
		case wbIsOff(effort):
			body["reasoning_effort"] = effort
		case resolved != "":
			body["reasoning_effort"] = resolved
		case body["reasoning_effort"] == nil:
			body["reasoning_effort"] = effort // 目录无档位信息时透传客户端原值
		}
		wbClearEffortAliases(body)
	} else {
		// 未显式给档位：无 thinking 或 thinking.type=enabled 时注入目录默认
		tt := strings.TrimSpace(wbThinkingType(body))
		if (tt == "" || strings.EqualFold(tt, "enabled")) && m != nil &&
			m.Default != "" && !wbIsOff(m.Default) {
			body["reasoning_effort"] = m.Default
		}
		// deepseek 系无目录默认时按官方 CLI 行为落 high（客户端未声明 thinking
		// 即开启思考）；显式 off/已有档位不受影响
		if isDeepSeek {
			if s, _ := body["reasoning_effort"].(string); s == "" {
				body["reasoning_effort"] = "high"
			}
		}
	}
	final, _ := body["reasoning_effort"].(string)
	if final != "" && !wbIsOff(final) {
		if isDeepSeek {
			th, _ := body["thinking"].(map[string]any)
			if th == nil {
				th = map[string]any{}
				body["thinking"] = th
			}
			if t, _ := th["type"].(string); strings.TrimSpace(t) == "" {
				th["type"] = "enabled"
			}
		}
		if _, ok := body["reasoning_summary"]; !ok {
			body["reasoning_summary"] = "auto"
		}
		wbBackfillReasoning(body, isDeepSeek, true)
	} else {
		wbBackfillReasoning(body, isDeepSeek, false)
	}
}

func wbClearEffortAliases(body map[string]any) {
	delete(body, "reasoningEffort")
	if r, ok := body["reasoning"].(map[string]any); ok {
		delete(r, "effort")
		if len(r) == 0 {
			delete(body, "reasoning")
		}
	}
}

// wbBackfillReasoning 历史 assistant 消息的思考痕迹回填：上游校验 reasoning 长度，
// 空痕迹补一个空格占位。all=false 时仅当 deepseek 且已有消息带非空痕迹才回填。
func wbBackfillReasoning(body map[string]any, isDeepSeek bool, all bool) {
	if !all && !isDeepSeek {
		return
	}
	anyTrace := false
	for _, mm := range wbMessages(body) {
		m, ok := mm.(map[string]any)
		if !ok || !wbIsRole(m, "assistant") {
			continue
		}
		if s, _ := m["reasoning_content"].(string); strings.TrimSpace(s) != "" {
			anyTrace = true
			break
		}
		if s, _ := m["reasoning"].(string); strings.TrimSpace(s) != "" {
			anyTrace = true
			break
		}
	}
	if !all && !anyTrace {
		return
	}
	for _, mm := range wbMessages(body) {
		m, ok := mm.(map[string]any)
		if !ok || !wbIsRole(m, "assistant") {
			continue
		}
		rc, hasRC := m["reasoning_content"].(string)
		r, hasR := m["reasoning"].(string)
		if !hasRC && hasR && r != "" {
			rc = r
			m["reasoning_content"] = rc
		}
		if strings.TrimSpace(rc) == "" {
			m["reasoning"] = " " // 空痕迹占位
		} else if !hasR || r == "" {
			m["reasoning"] = rc
		}
	}
}

// ── 模型目录 ──────────────────────────────────────────────────────────────

// wbCatalog 一个 provider 的模型目录（远端拉取或内置兜底）
type wbCatalog struct {
	Meta   map[string]*wbModelMeta // 键 = 小写模型 id
	IDs    []string                // 保留上游顺序，用于后台提示
	Remote bool                    // 是否远端目录（false = 内置兜底）
	At     time.Time
}

func (c *wbCatalog) meta(id string) *wbModelMeta {
	return c.Meta[strings.ToLower(strings.TrimSpace(id))]
}

// wbBuiltinIDs 内置模型清单（目录拉取失败时的兜底，逐字照抄适配器 builtin_models；
// 构造目录时同样过一遍准入过滤，default-1.x / hunyuan-chat / kimi-k2-instruct-taiji
// 会被滤掉——与适配器行为一致）
var wbBuiltinIDs = []string{
	"auto", "default",
	"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v3-2-volc",
	"glm-5.1", "glm-5.0", "glm-5.0-turbo", "glm-5v-turbo", "glm-4.7", "glm-4.6", "glm-4.6v",
	"minimax-m2.5", "minimax-m2.7", "minimax-m3-play",
	"kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2-instruct-taiji",
	"hy3-preview", "hy3-preview-agent", "hunyuan-chat", "hunyuan-2.0-instruct", "hunyuan-2.0-thinking",
	"default-1.1", "default-1.2",
}

// wbExcludedPrefixes 内置兜底清单的准入过滤：这些前缀的模型不对外提供。
// （仅用于内置清单；远端目录改由 data.agents 的 cli 清单驱动，见 wbFetchCatalog）
var wbExcludedPrefixes = []string{
	"completion-", "codewise-", "hunyuan-image", "hunyuan-3b", "hunyuan-7b",
	"deepseek-r1-0528", "deepseek-v3-0324", "kimi-k2-instruct", "default-1.",
}

// wbAdmitModel 内置兜底清单的准入过滤（适配器 is_chat_model 的移植）：
// 排除前缀、supportsExtra 为 JS 真值、maxOutputTokens 缺失或 <16000 的都不要。
func wbAdmitModel(id string, m map[string]any) bool {
	for _, p := range wbExcludedPrefixes {
		if strings.HasPrefix(id, p) {
			return false
		}
	}
	if b, ok := m["disabled"].(bool); ok && b {
		return false
	}
	if wbJSTruthy(m["supportsExtra"]) {
		return false
	}
	f, ok := m["maxOutputTokens"].(float64)
	return ok && f >= 16000
}

// wbJSTruthy JS 真值判定（目录过滤用）
func wbJSTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	default: // 数组/对象恒真
		return true
	}
}

// wbFetchCatalog 拉取远端目录：GET {endpoint}/v3/config。目录算法对齐官方 CLI
// （CloudProductManagerImpl 契约，参照 ccLoad FetchModels）：data.models 只是模型
// 定义总目录，可调用清单以 data.agents 里 name=="cli" 的 models 为准——与定义按
// 完整 ID 匹配，disabled:true 剔除，非空 availableModels 取交集，去重。不维护固定
// 排除名单。CLI 清单缺失或筛后为空视为目录无效（调用方退回内置兜底清单）。
// 注意目录筛过不等于账号余额/权限/服务实时健康（上游 11102 = service info not
// found 表示模型无服务配置，由错误分类按换候选处理）。
func wbFetchCatalog(endpoint string, e *wbEdition, cred *WorkBuddyCred) (*wbCatalog, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header = wbFrameworkHeader(e, cred)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("目录接口返回 HTTP %d: %s", resp.StatusCode, errSnippet(b))
	}
	var v struct {
		Code    *float64       `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("目录响应不是合法 JSON: %v", err)
	}
	if v.Code != nil && *v.Code != 0 {
		return nil, fmt.Errorf("目录接口返回 code %v: %s", *v.Code, v.Message)
	}
	cliList := wbCLIModelSet(v.Data)
	if len(cliList) == 0 {
		return nil, fmt.Errorf("目录缺少 cli 渠道模型清单（agents.name=cli），可能是账号未授权 CLI 渠道")
	}
	cli := make(map[string]bool, len(cliList))
	for _, id := range cliList {
		cli[id] = true
	}
	avail := wbStringSet(v.Data["availableModels"])
	cat := &wbCatalog{Meta: map[string]*wbModelMeta{}, Remote: true, At: time.Now()}
	models, _ := v.Data["models"].([]any)
	for _, mm := range models {
		m, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if !cli[key] {
			continue // 不在 cli 可调用清单
		}
		if b, ok := m["disabled"].(bool); ok && b {
			continue
		}
		if len(avail) > 0 && !avail[key] {
			continue // 账号 availableModels 未开放
		}
		if _, dup := cat.Meta[key]; dup {
			continue
		}
		cat.Meta[key] = wbMetaFromModel(m)
		cat.IDs = append(cat.IDs, key)
	}
	if len(cat.IDs) == 0 {
		return nil, fmt.Errorf("目录筛后没有可用模型（cli 清单 %d 个），可能是账号未授权", len(cli))
	}
	return cat, nil
}

// wbAgentCLIModels 从 agents 数组里取 name=="cli" 条目的 models（去空白、小写）
func wbAgentCLIModels(agents []any) []string {
	for _, a := range agents {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := am["name"].(string); !strings.EqualFold(strings.TrimSpace(n), "cli") {
			continue
		}
		ms, _ := am["models"].([]any)
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			if s, ok := m.(string); ok {
				if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// wbCLIModelSet 目录数据里的 CLI 可调用模型清单：data.agents[].name=="cli" 的
// models；兼容历史嵌套形态 data.agent.agents（未认证响应里出现过）。
func wbCLIModelSet(data map[string]any) []string {
	if agents, ok := data["agents"].([]any); ok {
		if l := wbAgentCLIModels(agents); len(l) > 0 {
			return l
		}
	}
	if agent, ok := data["agent"].(map[string]any); ok {
		if agents, ok := agent["agents"].([]any); ok {
			return wbAgentCLIModels(agents)
		}
	}
	return nil
}

// wbStringSet 任意 JSON 数组转小写字符串集合（availableModels 用）
func wbStringSet(v any) map[string]bool {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	set := make(map[string]bool, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
				set[s] = true
			}
		}
	}
	return set
}

// wbBuiltinCatalog 内置兜底目录（无推理元数据，档位字段将原样透传）。
// 内置清单自带准入过滤（wbAdmitModel）；远端目录不走该过滤，由 cli 清单驱动。
func wbBuiltinCatalog() *wbCatalog {
	cat := &wbCatalog{Meta: map[string]*wbModelMeta{}, At: time.Now()}
	for _, id := range wbBuiltinIDs {
		if !wbAdmitModel(id, map[string]any{"maxOutputTokens": builtinMaxOut[id]}) {
			continue
		}
		cat.Meta[id] = &wbModelMeta{}
		cat.IDs = append(cat.IDs, id)
	}
	return cat
}

// builtinMaxOut 内置清单各模型的 maxOutputTokens（准入过滤用，逐字照抄）
var builtinMaxOut = map[string]float64{
	"auto": 32000, "default": 24000,
	"deepseek-v4-pro": 50000, "deepseek-v4-flash": 50000, "deepseek-v4.1-flash": 50000,
	"deepseek-v3-2-volc": 32000,
	"glm-5.1":            48000, "glm-5.0": 48000, "glm-5.0-turbo": 48000, "glm-5v-turbo": 38000,
	"glm-4.7": 48000, "glm-4.6": 32000, "glm-4.6v": 32000,
	"minimax-m2.5": 48000, "minimax-m2.7": 48000, "minimax-m3-play": 48000,
	"kimi-k2.6": 32000, "kimi-k2.5": 32000, "kimi-k2-thinking": 32000, "kimi-k2-instruct-taiji": 8192,
	"hy3-preview": 64000, "hy3-preview-agent": 64000, "hunyuan-chat": 8192,
	"hunyuan-2.0-instruct": 16000, "hunyuan-2.0-thinking": 24000,
	"default-1.1": 8192, "default-1.2": 24000,
}

// wbInvalidateCatalog 丢弃目录缓存（凭证更新或手动刷新时）
func (s *Server) wbInvalidateCatalog(providerID string) {
	s.wbMu.Lock()
	delete(s.wbCats, providerID)
	s.wbMu.Unlock()
}

// wbCatalogTTL 远端目录的缓存时长：上游新增/下线模型后最多这么久生效
const wbCatalogTTL = 6 * time.Hour

// wbUseCatalog 缓存条目是否可直接复用：远端目录有 TTL，内置兜底目录 10 分钟后重试拉取
func wbUseCatalog(c *wbCatalog) bool {
	if c == nil || len(c.IDs) == 0 {
		return false // 空目录不作为有效目录
	}
	if c.Remote {
		return time.Since(c.At) < wbCatalogTTL
	}
	return time.Since(c.At) < 10*time.Minute
}

// wbCatalogFor 取该 provider 的目录缓存；失效或为空时重新拉取，失败退回内置清单。
// 拉取失败或返回空目录时缓存内置兜底（Remote=false，10 分钟后重试），
// 避免把一次失败当成长期有效的远端目录。
func (s *Server) wbCatalogFor(p Provider) *wbCatalog {
	e, cred := wbOf(p)
	if e == nil || cred == nil || cred.AccessToken == "" {
		return wbBuiltinCatalog()
	}
	s.wbMu.Lock()
	c := s.wbCats[p.ID]
	s.wbMu.Unlock()
	if wbUseCatalog(c) {
		return c
	}
	nc, err := wbFetchCatalog(wbEndpoint(p), e, cred)
	if err != nil || len(nc.IDs) == 0 {
		nc = wbBuiltinCatalog()
	}
	s.wbMu.Lock()
	if s.wbCats == nil {
		s.wbCats = map[string]*wbCatalog{}
	}
	s.wbCats[p.ID] = nc
	s.wbMu.Unlock()
	return nc
}

// ── token 续期 ────────────────────────────────────────────────────────────

// wbRefreshWindow token 临期窗口：距过期不足 5 分钟时提前续期
const wbRefreshWindow = 5 * time.Minute

func wbTokenExpiring(t time.Time) bool {
	return !t.IsZero() && time.Until(t) < wbRefreshWindow
}

// wbProviderLock 续期单飞锁：同一 provider 的并发续期只发一次上游请求
func (s *Server) wbProviderLock(id string) *sync.Mutex {
	s.wbMu.Lock()
	defer s.wbMu.Unlock()
	if s.wbLocks == nil {
		s.wbLocks = map[string]*sync.Mutex{}
	}
	l, ok := s.wbLocks[id]
	if !ok {
		l = &sync.Mutex{}
		s.wbLocks[id] = l
	}
	return l
}

// wbForgetProvider 丢弃该 provider 的续期锁与目录缓存（删除 Provider 时调用）
func (s *Server) wbForgetProvider(id string) {
	s.wbMu.Lock()
	delete(s.wbLocks, id)
	delete(s.wbCats, id)
	s.wbMu.Unlock()
}

// wbRefreshToken 刷新 WorkBuddy 凭证并回写。force 时无视临期窗口（上游 401 后强制换新）。
// refresh token 是一次性的，轮换后旧值即失效，因此按 provider 加锁单飞；
// 等锁的请求拿到锁后先复查凭证（可能已被别人刷新）。
func (s *Server) wbRefreshToken(p Provider, force bool) (*WorkBuddyCred, error) {
	lock := s.wbProviderLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	cur, ok := s.store.Provider(p.ID)
	if !ok {
		return nil, fmt.Errorf("provider 已删除")
	}
	e, cred := wbOf(cur)
	if e == nil || cred == nil {
		return nil, fmt.Errorf("provider 缺少 WorkBuddy 凭证")
	}
	if !force && !wbTokenExpiring(cred.expires()) {
		return cred, nil
	}
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("缺少 refreshToken，无法续期，请重新导入凭证")
	}

	req, err := http.NewRequest(http.MethodPost, wbEndpoint(cur)+"/v2/plugin/auth/token/refresh", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header = wbFrameworkHeader(e, cred)
	req.Header.Set("X-Refresh-Token", cred.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	timeout := 30 * time.Second
	if !e.intl {
		timeout = 8 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("续期请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, fmt.Errorf("续期接口返回 HTTP %d（通常该账号端点指向了错误地区）", resp.StatusCode)
	}
	var v struct {
		Code    *float64       `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("续期响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	if v.Code != nil && *v.Code != 0 {
		if *v.Code == 12153 {
			return nil, fmt.Errorf("refresh token 已失效（code 12153），请重新导入凭证")
		}
		return nil, fmt.Errorf("续期返回 code %v: %s", *v.Code, v.Message)
	}
	nc := *cred
	if at, _ := v.Data["accessToken"].(string); at != "" {
		nc.AccessToken = at
	} else {
		return nil, fmt.Errorf("续期响应缺少 accessToken")
	}
	if rt, _ := v.Data["refreshToken"].(string); rt != "" {
		nc.RefreshToken = rt
	}
	if d, _ := v.Data["domain"].(string); d != "" {
		nc.Domain = d // 官方续期响应会回 domain，跟随更新（仅控制面请求使用）
	}
	nc.ExpiresAt = wbTimePtr(wbExpiryOf(v.Data, cred.expires(), "expiresAt", "expiresIn"))
	nc.RefreshExpiresAt = wbTimePtr(wbExpiryOf(v.Data, cred.refreshExpires(), "refreshExpiresAt", "refreshExpiresIn"))
	// 轻量身份校验：新 accessToken 的 JWT sub 应仍是同一账号（仅解码，不验签）
	if sub := wbJWTSub(nc.AccessToken); sub != "" && nc.UID != "" && sub != nc.UID {
		return nil, fmt.Errorf("续期后账号身份变化（sub=%s ≠ uid=%s），已放弃回写", sub, nc.UID)
	}
	if !s.store.UpdateWBAuth(p.ID, &nc) {
		return nil, fmt.Errorf("续期结果未能落盘（provider 可能已删除）")
	}
	return &nc, nil
}

// wbExpiryOf 显式毫秒时间戳优先，否则按 expiresIn 秒现算；两者都没有时保留旧值
func wbExpiryOf(data map[string]any, old time.Time, atKey, inKey string) time.Time {
	if f, ok := data[atKey].(float64); ok && f > 0 {
		return time.UnixMilli(int64(f))
	}
	if f, ok := data[inKey].(float64); ok && f > 0 {
		return time.Now().Add(time.Duration(f) * time.Second)
	}
	return old
}

// wbTimePtr 零值时刻返回 nil（序列化时省略，前端不显示 0001-01-01）
func wbTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// wbJWTSub 解码 JWT payload 的 sub（仅用于身份核对，不验签）
func wbJWTSub(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var v struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &v) != nil {
		return ""
	}
	return v.Sub
}

// ── 错误分类 ──────────────────────────────────────────────────────────────

type wbErrClass int

const (
	wbErrTokenExpired   wbErrClass = iota // 401 / 12153：续期后重试
	wbErrQuota                            // 限流/额度：静音该模型条目
	wbErrContentBlocked                   // 请求被上游拒绝（参数/内容）：换候选，不静音
	wbErrFatal                            // 其余：换候选
)

// wbClassify 按状态码 + 业务码分类上游错误，并尽力解析限流恢复时刻。
// 业务码递归查找：节点自身 → data → error → response.data（≤6 层，首个 ≥1000）。
func wbClassify(status int, body []byte) (wbErrClass, int64, string, time.Time) {
	var v any
	json.Unmarshal(body, &v)
	code, node := wbFindCode(v)
	msg := wbErrorMessage(v, node, body)
	lower := strings.ToLower(msg)
	switch {
	case status == 401 || code == 12153:
		return wbErrTokenExpired, code, msg, time.Time{}
	case (code == 11140 && (strings.Contains(lower, "request illegal") ||
		strings.Contains(lower, "account has been restricted") ||
		strings.Contains(lower, "unusual activity") ||
		strings.Contains(msg, "账号存在异常行为"))) ||
		code == 14015 || code == 14016 || code == 14017 || code == 11115 ||
		code == 11101 || code == 11133 || code == 11158:
		return wbErrFatal, code, msg, time.Time{}
	case code == 11128:
		// 上游把请求形态/参数拒绝也归到这个码（与 developer 角色折 system 同一来源），
		// 语义不是「内容拦截」：只换候选、不静音
		return wbErrContentBlocked, code, msg, time.Time{}
	case status == 402 || status == 429 ||
		code == 14001 || code == 14012 || code == 14013 || code == 14014 ||
		code == 14018 || code == 14019 || code == 14003 || code == 10105 ||
		(code >= 6000 && code <= 6008) ||
		strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate-limiting") ||
		strings.Contains(lower, "rate-limited") || strings.Contains(lower, "too many requests") ||
		strings.Contains(msg, "请求过于频繁") || strings.Contains(msg, "限流"):
		return wbErrQuota, code, msg, wbParseResetAt(msg)
	default:
		return wbErrFatal, code, msg, time.Time{}
	}
}

func wbFindCode(v any) (int64, any) {
	c, n, _ := wbFindCodeAt(v, 0)
	return c, n
}

func wbFindCodeAt(v any, depth int) (int64, any, bool) {
	if depth > 6 {
		return 0, nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, nil, false
	}
	if c, ok := wbCodeVal(m["code"]); ok {
		return c, m, true
	}
	for _, k := range []string{"data", "error"} {
		if c, n, ok := wbFindCodeAt(m[k], depth+1); ok {
			return c, n, true
		}
	}
	if resp, ok := m["response"].(map[string]any); ok {
		if dm, ok := resp["data"].(map[string]any); ok {
			if c, ok := wbCodeVal(dm["code"]); ok {
				return c, dm, true
			}
		}
	}
	return 0, nil, false
}

// wbCodeVal 业务码：数字或数字字符串，≥1000 才算
func wbCodeVal(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x >= 1000 && x == math.Trunc(x) {
			return int64(x), true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil && n >= 1000 {
			return n, true
		}
	}
	return 0, false
}

// wbErrorMessage 业务节点上的 message/msg/displayMsg，其次 error.message，最后原始响应
func wbErrorMessage(v any, node any, body []byte) string {
	if m, ok := node.(map[string]any); ok {
		for _, k := range []string{"message", "msg", "displayMsg"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	if m, ok := v.(map[string]any); ok {
		if e, ok := m["error"].(map[string]any); ok {
			if s, ok := e["message"].(string); ok && s != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(truncateRunes(string(body), 160))
	if s == "" {
		s = "上游错误"
	}
	return s
}

// wbUTC8 限流文案里的恢复时刻按北京时间表达
var wbUTC8 = time.FixedZone("UTC+8", 8*3600)

// wbParseResetAt 解析限流文案中的恢复时刻，两种形态：
//
//	· "2026-09-11 19:43:46 UTC+8"（字节级扫描，避免切断中文）
//	· "当日/今日 HH:MM"（北京时间今日时刻，已过则视为没有信息）
func wbParseResetAt(msg string) time.Time {
	b := []byte(msg)
	for i := 0; i+19 <= len(b); i++ {
		if t, ok := wbScanReset(b, i); ok {
			return t
		}
	}
	if t, ok := wbScanTodayReset(msg); ok && t.After(time.Now()) {
		return t
	}
	return time.Time{}
}

// wbScanReset 从 b[i] 起尝试匹配 "dddd-dd-dd \s+ dd:dd:dd \s* UTC+8"
func wbScanReset(b []byte, i int) (time.Time, bool) {
	for k := 0; k < 10; k++ {
		want := byte('-')
		if k != 4 && k != 7 {
			want = '0'
		}
		c := b[i+k]
		if want == '-' {
			if c != '-' {
				return time.Time{}, false
			}
		} else if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	j := i + 10
	if j >= len(b) || !wbIsSpaceByte(b[j]) {
		return time.Time{}, false
	}
	for j < len(b) && wbIsSpaceByte(b[j]) {
		j++
	}
	if j+8 > len(b) {
		return time.Time{}, false
	}
	t0 := j // 时间 dd:dd:dd 的起点
	if b[j+2] != ':' || b[j+5] != ':' ||
		!wbIsDigitAt(b, j) || !wbIsDigitAt(b, j+1) ||
		!wbIsDigitAt(b, j+3) || !wbIsDigitAt(b, j+4) ||
		!wbIsDigitAt(b, j+6) || !wbIsDigitAt(b, j+7) {
		return time.Time{}, false
	}
	j += 8
	for j < len(b) && wbIsSpaceByte(b[j]) {
		j++
	}
	if !bytes.HasPrefix(b[j:], []byte("UTC+8")) {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(string(b[i : i+4]))
	mo, _ := strconv.Atoi(string(b[i+5 : i+7]))
	d, _ := strconv.Atoi(string(b[i+8 : i+10]))
	hh, _ := strconv.Atoi(string(b[t0 : t0+2]))
	mi, _ := strconv.Atoi(string(b[t0+3 : t0+5]))
	ss, _ := strconv.Atoi(string(b[t0+6 : t0+8]))
	if y < 2020 || mo < 1 || mo > 12 || d < 1 || d > 31 || hh > 23 || mi > 59 || ss > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, hh, mi, ss, 0, wbUTC8)
	// 溢出字段（如 02-31）被 time.Date 归一到下月，视为无效
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}

// wbScanTodayReset "当日/今日 … HH:MM"：取其后 16 个字符内第一个 H[:MM] 时刻
func wbScanTodayReset(msg string) (time.Time, bool) {
	pos := strings.Index(msg, "当日")
	if p2 := strings.Index(msg, "今日"); pos < 0 || (p2 >= 0 && p2 < pos) {
		pos = p2
	}
	if pos < 0 {
		return time.Time{}, false
	}
	s := msg[pos:]
	if r := []rune(s); len(r) > 18 {
		s = string(r[:18])
	}
	for i := 0; i+4 <= len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' && j-i < 2 {
			j++
		}
		if j >= len(s) || s[j] != ':' || j+3 > len(s) {
			continue
		}
		if s[j+1] < '0' || s[j+1] > '9' || s[j+2] < '0' || s[j+2] > '9' {
			continue
		}
		if j+3 < len(s) && s[j+3] >= '0' && s[j+3] <= '9' { // 分钟后不能再跟数字
			continue
		}
		hh, _ := strconv.Atoi(s[i:j])
		mi, _ := strconv.Atoi(s[j+1 : j+3])
		if hh >= 24 || mi >= 60 {
			continue
		}
		now := time.Now().In(wbUTC8)
		return time.Date(now.Year(), now.Month(), now.Day(), hh, mi, 0, 0, wbUTC8), true
	}
	return time.Time{}, false
}

func wbIsDigitAt(b []byte, i int) bool { return b[i] >= '0' && b[i] <= '9' }

func wbIsSpaceByte(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// ── SSE 节拍过滤与非流式聚合 ──────────────────────────────────────────────

// wbSSEFilter 上游 SSE 的流式整形，按帧（空行分隔）处理：
//
//	· 「整片只有换行」的 content 分片是上游插在正文之间的节拍，整帧丢弃——
//	  模型真实输出的纯换行分片与节拍无法区分，也会被丢，与适配器行为一致；
//	· 思考流被上游拆成「一个分片一个 token」、中间夹纯空白的节拍分片，直接
//	  透传会让按分片渲染思考的客户端（ZCode 等）一词一行刷屏——纯 reasoning
//	  增量帧全部进缓冲，等思考结束（首个带 data 载荷的非 reasoning 帧：正文/
//	  工具调用/收尾/用量，或流结束）再合成一个分片一次性放行，保证同一段思考
//	  在客户端只出现一个思考条目。持有期间定期发 SSE 注释行（: keepalive）保活
//	  连接——注释行不产生事件，客户端渲染不受影响。上游中途的心跳注释帧同样
//	  透传、不打断持有。其余字节原样放行。
type wbSSEFilter struct {
	r   io.Reader
	buf []byte // 已读入、待切帧
	out []byte // 已放行、待交付
	eof bool
	err error // 底层读错误：残余帧交付完后返回，避免中途断流被当成正常结束

	rsBuf  []byte    // 合并中的 reasoning 增量文本
	rsTmpl []byte    // 合成帧的 data 载荷模板（沿用首个 reasoning 帧的其余字段）
	rsLast time.Time // 最近一次保活注释的发出时刻（wbKeepaliveEvery 节流）
}

// wbMaxPending 单个未完成帧的最大缓冲：异常上游一直不发空行时防止内存无界增长；
// 也用作思考持有缓冲的上限（超限先放行一次，正常思考量远达不到）
const wbMaxPending = 4 << 20

// wbKeepaliveEvery 思考持有期间向客户端发送 SSE 注释行保活的间隔：长思考期间
// 没有任何事件下发，部分 HTTP 客户端有空闲超时。变量便于测试覆写。
var wbKeepaliveEvery = 10 * time.Second

func (f *wbSSEFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if start := wbFrameBoundary(f.buf); start >= 0 {
			frame := f.buf[:start]
			f.buf = f.buf[start:]
			f.handleFrame(frame)
			continue
		}
		if len(f.buf) > wbMaxPending {
			f.eof = true
			f.err = fmt.Errorf("单帧超过 %d 字节仍未结束，放弃该响应", wbMaxPending)
		}
		if f.eof {
			if len(f.buf) > 0 { // 流尾未以空行结束的残余（或超限帧）：按规则放行
				frame := f.buf
				f.buf = nil
				f.handleFrame(frame)
				continue
			}
			f.flushReasoning()
			if len(f.out) > 0 {
				break
			}
			if f.err != nil {
				return 0, f.err
			}
			return 0, io.EOF
		}
		if err := f.pump(); err != nil {
			f.eof = true
			if err != io.EOF {
				f.err = err
			}
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// pump 读一块数据追加进缓冲（切帧在 Read 的循环里进行）
func (f *wbSSEFilter) pump() error {
	tmp := make([]byte, 16*1024)
	n, err := f.r.Read(tmp)
	f.buf = append(f.buf, tmp[:n]...)
	return err
}

// handleFrame 处理一个完整帧：纯 reasoning 增量帧进合并缓冲（思考期间不放行），
// 无 data 载荷的注释帧（上游心跳）透传且不打断持有，content 节拍帧丢弃，其余
// （正文/工具调用/用量/[DONE]/未识别）先冲刷缓冲再原样放行——思考结束的标志
// 正是这些帧的到来
func (f *wbSSEFilter) handleFrame(frame []byte) {
	if d, payload, ok := wbReasoningChunk(frame); ok {
		now := time.Now()
		if f.rsTmpl == nil {
			f.rsTmpl, f.rsLast = payload, now
		}
		f.rsBuf = append(f.rsBuf, d...)
		if len(f.rsBuf) >= wbMaxPending { // 兜底：正常思考量远达不到
			f.flushReasoning()
			return
		}
		// 持有期间定期发 SSE 注释行：不产生事件，仅维持客户端连接活跃
		if now.Sub(f.rsLast) >= wbKeepaliveEvery && len(f.out) == 0 {
			f.out = append(f.out, ": keepalive\n\n"...)
			f.rsLast = now
		}
		return
	}
	if !bytes.Contains(frame, []byte("data:")) { // 注释/心跳帧：透传，不打断思考持有
		f.out = append(f.out, frame...)
		return
	}
	f.flushReasoning()
	if !wbNewlineBeat(frame) {
		f.out = append(f.out, frame...)
	}
}

// flushReasoning 把合并中的 reasoning 增量合成一个分片放行；只剩空白（连续节拍、
// 无正文）时丢弃。模板缺失或异常时兜底构造最小分片，保证增量不丢。
func (f *wbSSEFilter) flushReasoning() {
	buf, tmpl := f.rsBuf, f.rsTmpl
	f.rsBuf, f.rsTmpl = nil, nil
	if len(buf) == 0 || len(bytes.TrimSpace(buf)) == 0 {
		return
	}
	var v map[string]any
	if json.Unmarshal(tmpl, &v) != nil || v == nil {
		v = map[string]any{}
	}
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		choices = append(choices, map[string]any{})
		v["choices"] = choices
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		choice = map[string]any{}
		choices[0] = choice
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
		choice["delta"] = delta
	}
	delta["reasoning_content"] = string(buf)
	if b, err := json.Marshal(v); err == nil {
		f.out = append(f.out, "data: "...)
		f.out = append(f.out, b...)
		f.out = append(f.out, '\n', '\n')
	}
}

// wbReasoningChunk 该帧是否为纯 reasoning 增量帧（仅 delta.reasoning_content 非空，
// 无正文/工具调用/收尾/用量）。是则返回增量文本与完整 data 载荷（合成帧的模板）。
func wbReasoningChunk(frame []byte) (string, []byte, bool) {
	var payload []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		d, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			return "", nil, false // 混有其他行（id/event/注释等），不合并，原样放行
		}
		payload = append(payload, bytes.TrimPrefix(d, []byte(" "))...)
	}
	if len(payload) == 0 {
		return "", nil, false
	}
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Refusal          string `json:"refusal"`
				FunctionCall     any    `json:"function_call"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(payload, &v) != nil || len(v.Choices) == 0 {
		return "", nil, false
	}
	ch := v.Choices[0]
	if ch.Delta.ReasoningContent == "" || ch.Delta.Content != "" || ch.Delta.Refusal != "" ||
		ch.Delta.FunctionCall != nil || len(ch.Delta.ToolCalls) > 0 || ch.FinishReason != "" ||
		(len(v.Usage) > 0 && string(v.Usage) != "null") {
		return "", nil, false
	}
	return ch.Delta.ReasoningContent, payload, true
}

// wbFrameBoundary 第一处帧边界（空行）的下一字节位置；没有完整帧返回 -1
func wbFrameBoundary(b []byte) int {
	i1 := bytes.Index(b, []byte("\n\n"))
	i2 := bytes.Index(b, []byte("\r\n\r\n"))
	if i1 < 0 && i2 < 0 {
		return -1
	}
	if i1 < 0 || (i2 >= 0 && i2 < i1) {
		return i2 + 4
	}
	return i1 + 2
}

// wbNewlineBeat 该帧是否为节拍帧：只由 data: 行组成，且载荷是
// choices[0].delta.content 全为换行的分片
func wbNewlineBeat(frame []byte) bool {
	var payload []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		d, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			return false // 混有其他行（id/event/注释等），原样放行
		}
		payload = append(payload, bytes.TrimPrefix(d, []byte(" "))...)
	}
	if len(payload) == 0 {
		return false
	}
	var v struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &v) != nil || len(v.Choices) == 0 {
		return false
	}
	c := v.Choices[0].Delta.Content
	if c == "" {
		return false
	}
	for _, r := range c {
		if r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// wbAgg 把上游流式响应聚合为一个 chat.completion（上游不支持 stream:false，
// 非流式客户端由本网关代为聚合）
type wbAgg struct {
	lines     []byte
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*wbToolAcc
	finish    string
	usageRaw  json.RawMessage
}

type wbToolAcc struct {
	ID     string
	FnName string
	Args   strings.Builder
}

// Write 消费过滤后的 SSE 字节流（按行切出 data: 载荷）
func (a *wbAgg) Write(p []byte) (int, error) {
	a.lines = append(a.lines, p...)
	for {
		i := bytes.IndexByte(a.lines, '\n')
		if i < 0 {
			break
		}
		line := a.lines[:i]
		a.lines = a.lines[i+1:]
		line = bytes.TrimSuffix(line, []byte("\r"))
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			a.feed(bytes.TrimPrefix(d, []byte(" ")))
		}
	}
	return len(p), nil
}

// feed 处理一条 data: 载荷
func (a *wbAgg) feed(d []byte) {
	if bytes.Equal(bytes.TrimSpace(d), []byte("[DONE]")) {
		return
	}
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(d, &v) != nil {
		return
	}
	if len(v.Usage) > 0 && string(v.Usage) != "null" {
		a.usageRaw = v.Usage
	}
	if len(v.Choices) == 0 {
		return
	}
	ch := v.Choices[0]
	a.content.WriteString(ch.Delta.Content)
	a.reasoning.WriteString(ch.Delta.ReasoningContent)
	for _, tc := range ch.Delta.ToolCalls {
		if a.tools == nil {
			a.tools = map[int]*wbToolAcc{}
		}
		acc := a.tools[tc.Index]
		if acc == nil {
			acc = &wbToolAcc{}
			a.tools[tc.Index] = acc
		}
		if tc.ID != "" {
			acc.ID = tc.ID
		}
		if tc.Function.Name != "" {
			acc.FnName = tc.Function.Name
		}
		acc.Args.WriteString(tc.Function.Arguments)
	}
	if ch.FinishReason != "" {
		a.finish = ch.FinishReason
	}
}

// result 生成 chat.completion 响应与记账用量
func (a *wbAgg) result(model string) ([]byte, Usage) {
	msg := map[string]any{"role": "assistant"}
	// content 为 null 而非缺失：仅在纯 tool_calls 分支省略时，部分严格 SDK 会取
	// message.content 拿到 undefined 后报错（OpenAI 规范此处是 null）
	if a.content.Len() > 0 {
		msg["content"] = a.content.String()
	} else if len(a.tools) == 0 {
		msg["content"] = ""
	} else {
		msg["content"] = nil
	}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		idx := a.sortedToolIdx()
		calls := make([]any, 0, len(idx))
		for _, i := range idx {
			t := a.tools[i]
			calls = append(calls, map[string]any{
				"id": t.ID, "type": "function",
				"function": map[string]any{"name": t.FnName, "arguments": t.Args.String()},
			})
		}
		msg["tool_calls"] = calls
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + wbMessageID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}
	if a.usageRaw != nil {
		resp["usage"] = a.usageRaw
	}
	b, _ := json.Marshal(resp)
	return b, wbUsageOf(a.usageRaw)
}

// wbUsageOf 解析 usage 对象为记账用量（兼容 chat/responses 两套字段名及缓存分项）
func wbUsageOf(raw json.RawMessage) Usage {
	return parseUsageObject(raw)
}

// ── 候选处理入口（由代理循环调用） ────────────────────────────────────────

// wbBodyStream 客户端是否要求流式（默认非流式）
func wbBodyStream(body []byte) bool {
	var t struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &t)
	return t.Stream
}

// wbPrepareBody 出站准备：归一化 → 推理档位 → 注入首条 system → 强制 stream:true
// （上游不支持非流式，非流式客户端由 wbAttempt 聚合）
func (s *Server) wbPrepareBody(e *wbEdition, c Candidate, sendBody []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(sendBody, &m); err != nil {
		return nil, err
	}
	uid := ""
	if c.Provider.WB != nil {
		uid = c.Provider.WB.UID
	}
	wbNormalizeOutbound(m, uid)
	cat := s.wbCatalogFor(c.Provider)
	wbReasoningNormalize(m, cat.meta(c.Model.Name), c.Model.Name)
	wbEnsureLeadingSystem(m)
	m["stream"] = true
	return json.Marshal(m)
}

// wbAttempt 处理一个 WorkBuddy 候选的完整生命周期：懒续期 → 出站准备 → 专用请求头
// → 发送（凭证失效时强制续期后原候选重试一次）→ 错误分类（限流按上游恢复时刻静音）
// → 流式节拍过滤透传 / 非流式聚合。成功时已向客户端写出响应并返回 nil；
// 失败时返回错误描述（并行名额已释放、失败日志已记，调用方换下一个候选）。
// responsesMode 时入站是 /v1/responses 协议：先翻成 chat 出站，回程渲染回
// responses 事件流或聚合为 response 对象（见 workbuddy_responses.go）。
func (s *Server) wbAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *requestTiming, sm *usageSample) error {
	defer s.store.Release(c.Model.ID)
	e, cred := wbOf(c.Provider)
	if e == nil || cred == nil || cred.AccessToken == "" {
		err := fmt.Errorf("未导入 WorkBuddy 凭证")
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name, Message: err.Error()})
		return err
	}
	wantStream := wbBodyStream(sendBody)
	src := sendBody
	if responsesMode {
		chat, err := wbResponsesToChat(sendBody)
		if err != nil {
			s.logFail(LogEntry{Level: "error", Kind: kindBadRequest, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "/responses 请求体翻译失败", Detail: err.Error()})
			return err
		}
		if src, err = json.Marshal(chat); err != nil {
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "构造 WorkBuddy 出站请求失败", Detail: err.Error()})
			return err
		}
	}
	body, err := s.wbPrepareBody(e, c, src)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "构造 WorkBuddy 出站请求失败", Detail: err.Error()})
		return err
	}
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, wbEndpoint(c.Provider)+"/v2/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = wbChatHeader(e, cred, wbEndpoint(c.Provider))
		return s.client.Do(req)
	}
	// 懒续期：临期自动刷新，失败不阻断（沿用现有 token，上游报失效时再强制刷新）
	if cred.RefreshToken != "" && wbTokenExpiring(cred.expires()) {
		if nc, err := s.wbRefreshToken(c.Provider, false); err == nil {
			cred = nc
		} else {
			s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "WorkBuddy token 临期续期失败（沿用现有 token）", Detail: err.Error()})
		}
	}
	// 发请求并分类。凭证失效（HTTP 401 或上游把 12153 包在 200/400 里）时强制续期
	// 一次并原候选重试；refreshed 保证不会反复续期。
	var resp *http.Response
	refreshed := false
	for {
		resp, err = do()
		if err != nil {
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "连接上游失败（网络错误或超时）", Detail: err.Error()})
			return err
		}
		// SSE 端点上收到 JSON 响应即错误信封（含 200 的情况）
		if resp.StatusCode < 400 && !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			break // 正常响应
		}
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		class, ferr := s.wbFailResponse(c, keyID, resp.StatusCode, eb)
		if class != wbErrTokenExpired {
			return ferr
		}
		// 凭证失效：续期后重试一次。没有 refresh token 或已重试过，记 error 并换候选
		if refreshed || cred.RefreshToken == "" {
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "WorkBuddy 凭证失效且无法自动续期，请重新导入凭证", Detail: errSnippet(eb)})
			return ferr
		}
		nc, rerr := s.wbRefreshToken(c.Provider, true)
		if rerr != nil {
			err := fmt.Errorf("凭证失效，续期失败: %v", rerr)
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "WorkBuddy 凭证失效且续期失败，请重新导入", Detail: rerr.Error()})
			return err
		}
		s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, Status: resp.StatusCode, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "WorkBuddy 凭证已失效，已自动续期并重试该候选"})
		cred = nc
		refreshed = true
	}

	filter := &wbSSEFilter{r: resp.Body}
	defer resp.Body.Close()
	// 首字节在过滤之前打点：上游发出的第一个 SSE 分片（含保活注释）即算响应到达
	upBody := &firstByteReader{r: filter, t: tm}

	if wantStream {
		wbCopyRespHeaders(w, resp.Header, true)
		w.WriteHeader(resp.StatusCode)
		if responsesMode {
			// chat 分片 → responses 事件流，边收边渲染
			rs := newWBRespStream(w, c.Model.Name)
			rs.begin()
			_, cerr := io.Copy(rs, upBody)
			usage := rs.finish(cerr)
			if sm != nil {
				*sm = tm.sample(usage, true)
			}
			if cerr != nil {
				s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "WorkBuddy 流式响应中断（已渲染部分 responses 事件）", Detail: cerr.Error()})
			}
			return nil
		}
		sn := &sseSniffer{w: flushWriter{w}}
		_, cerr := io.Copy(sn, upBody)
		usage := extractStreamUsage(sn.tail)
		if sm != nil {
			*sm = tm.sample(usage, true)
		}
		if cerr != nil {
			// 响应头已发出，无法换候选；记一条日志便于排查截断
			s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "WorkBuddy 流式响应中断（已透传部分内容）", Detail: cerr.Error()})
		}
		return nil
	}

	// 非流式：先聚合再写响应。中途断流时不提交 200，交给调用方换下一个候选。
	var agg wbAgg
	_, cerr := io.Copy(&agg, upBody)
	if cerr != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "WorkBuddy 响应中断，聚合失败", Detail: cerr.Error()})
		return fmt.Errorf("上游流式响应中断: %v", cerr)
	}
	var out []byte
	var usage Usage
	if responsesMode {
		out, usage = agg.responsesResult(c.Model.Name)
	} else {
		out, usage = agg.result(c.Model.Name)
	}
	wbCopyRespHeaders(w, resp.Header, false)
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	if sm != nil {
		*sm = tm.sample(usage, false)
	}
	return nil
}

// wbCopyRespHeaders 复制上游响应头。Content-Length 一律丢弃：流式会被节拍过滤改变
// 字节数，非流式要换成聚合后的 JSON 长度。非流式还要把 Content-Type 改正为 JSON
// （上游固定回 text/event-stream，照抄会让客户端按 SSE 解析），并去掉 SSE 的缓存头。
func wbCopyRespHeaders(w http.ResponseWriter, h http.Header, stream bool) {
	for k, vv := range h {
		if skipRespHeader(k) || http.CanonicalHeaderKey(k) == "Content-Length" ||
			http.CanonicalHeaderKey(k) == "Content-Type" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Cache-Control")
}

// wbFailResponse 分类上游错误并做相应处置（限流静音/日志），返回分类与给候选循环的错误。
// 调用方据分类决定是否续期重试（wbErrTokenExpired）。
func (s *Server) wbFailResponse(c Candidate, keyID string, status int, body []byte) (wbErrClass, error) {
	class, code, msg, resetAt := wbClassify(status, body)
	switch class {
	case wbErrQuota:
		d, reason := rateLimitMute, "WorkBuddy 限流/额度用尽"
		if !resetAt.IsZero() && time.Until(resetAt) > 0 {
			d = time.Until(resetAt)
			reason = fmt.Sprintf("WorkBuddy 限流，上游告知恢复时刻 %s", resetAt.Local().Format("01-02 15:04"))
		}
		if until, ok := s.store.MuteOn429(c.Model.ID, d, reason); ok {
			s.logFail(LogEntry{Level: "warn", Kind: kindMuted, Status: status, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: fmt.Sprintf("WorkBuddy 限流（code %d），已静音至 %s", code, until.Local().Format("01-02 15:04")),
				Detail:  errSnippet(body)})
		}
	case wbErrContentBlocked:
		s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, Status: status, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("WorkBuddy 拒绝请求（code %d），已换下一候选", code), Detail: errSnippet(body)})
	case wbErrTokenExpired:
		// 日志由调用方按「续期重试是否成功」决定级别，这里不记
	default:
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: status, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("WorkBuddy 上游返回 HTTP %d", status), Detail: errSnippet(body)})
	}
	return class, fmt.Errorf("上游返回 %d: %s", status, msg)
}
