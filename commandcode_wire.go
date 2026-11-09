package main

// Command Code 线协议：chat / responses ↔ CLI /alpha/generate。
//
// 出站（ccBuildBody）把 OpenAI chat completions 请求翻译成 CLI 的 envelope：
//   {config:{workingDir,date,environment,...}, memory, taste, skills,
//    permissionMode:"standard", params:{model,messages,tools,system,max_tokens,...}}
// 回程（ccRelay）逐行读取 JSON 事件（text-delta / reasoning-delta / tool-call /
// finish / error），翻译成 chat.completion.chunk 交给现有聚合器；responses 接入
// 协议复用 wbRespStream / wbAgg，不另写一套。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// ccChunk / ccDelta 是回程生成的 chat.completion.chunk。
type ccChunk struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []ccChoice `json:"choices"`
	Usage   any        `json:"usage,omitempty"`
}

type ccChoice struct {
	Index        int     `json:"index"`
	Delta        ccDelta `json:"delta"`
	FinishReason any     `json:"finish_reason"`
}

type ccDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []ccDeltaToolCall `json:"tool_calls,omitempty"`
}

type ccDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ── 出站请求 ──────────────────────────────────────────────────────────────

// ccBuildBody 把 chat completions 请求体翻译成 /alpha/generate 的 envelope。
func ccBuildBody(raw []byte, model string) ([]byte, error) {
	var top struct {
		Model               string           `json:"model"`
		Messages            []map[string]any `json:"messages"`
		Tools               []map[string]any `json:"tools"`
		MaxTokens           int64            `json:"max_tokens"`
		MaxCompletionTokens int64            `json:"max_completion_tokens"`
		Temperature         *float64         `json:"temperature"`
		ReasoningEffort     string           `json:"reasoning_effort"`
		Reasoning           *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if model == "" {
		model = top.Model
	}

	var system []string
	messages := make([]any, 0, len(top.Messages))
	names := map[string]string{} // tool_call_id → tool name
	var results []any
	flushResults := func() {
		if len(results) > 0 {
			messages = append(messages, map[string]any{"role": "tool", "content": results})
			results = nil
		}
	}

	for _, m := range top.Messages {
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			system = append(system, ccTextOf(m["content"]))
		case "tool":
			id, _ := m["tool_call_id"].(string)
			name := names[id]
			if name == "" {
				name = "unknown"
			}
			results = append(results, map[string]any{
				"type": "tool-result", "toolCallId": id, "toolName": name,
				"output": map[string]any{"type": "text", "value": ccTextOf(m["content"])},
			})
		case "assistant":
			flushResults()
			var parts []any
			if r := ccFirstString(m["reasoning_content"], m["reasoning"]); r != "" {
				parts = append(parts, map[string]any{"type": "reasoning", "text": r})
			}
			if t := ccTextOf(m["content"]); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					id, _ := call["id"].(string)
					fn, _ := call["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if id != "" {
						names[id] = name
					}
					parts = append(parts, map[string]any{
						"type": "tool-call", "toolCallId": id, "toolName": name,
						"input": ccParseArgs(fn["arguments"]),
					})
				}
			}
			if len(parts) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": parts})
			}
		default:
			flushResults()
			if parts := ccContentParts(m["content"]); len(parts) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": parts})
			}
		}
	}
	flushResults()

	google := ccIsGoogle(model)
	tools := make([]any, 0, len(top.Tools))
	for _, t := range top.Tools {
		if typ, _ := t["type"].(string); typ != "function" {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if google {
			params = ccGoogleSchema(params)
		}
		tools = append(tools, map[string]any{"name": name, "description": desc, "input_schema": params})
	}

	maxTok := top.MaxCompletionTokens
	if maxTok <= 0 {
		maxTok = top.MaxTokens
	}
	if maxTok <= 0 {
		maxTok = commandCodeDefaultMax
	}
	if maxTok > commandCodeMaxOutput {
		maxTok = commandCodeMaxOutput
	}

	params := map[string]any{
		"model":      model,
		"messages":   messages,
		"tools":      tools,
		"system":     strings.Join(system, "\n\n"),
		"max_tokens": maxTok,
		"stream":     true, // /alpha/generate 恒为流式
	}
	if top.Temperature != nil {
		params["temperature"] = *top.Temperature
	}
	effort := top.ReasoningEffort
	if effort == "" && top.Reasoning != nil {
		effort = top.Reasoning.Effort
	}
	// Go 的 /alpha/generate 不认 "none"/"off"：不发档位，用模型默认。
	if effort != "" && effort != "none" && effort != "off" {
		params["reasoning_effort"] = ccFitEffort(effort, ccEffortsByModel[model])
	}

	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	out := map[string]any{
		"config": map[string]any{
			"workingDir":    cwd,
			"date":          time.Now().Format("2006-01-02"),
			"environment":   ccEnvironment(),
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"params":         params,
	}
	return json.Marshal(out)
}

// ccTextOf 取消息内容里的纯文本（字符串或 text 部件数组）。
func ccTextOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := p["type"].(string); t {
			case "text", "input_text", "output_text", "summary_text":
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ccContentParts 把 chat 的 content 转成 CLI 的部件数组（text / image）。
func ccContentParts(v any) []any {
	var parts []any
	switch c := v.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": "text", "text": c})
		}
	case []any:
		for _, raw := range c {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := p["type"].(string); t {
			case "text", "input_text", "output_text":
				if s, ok := p["text"].(string); ok && s != "" {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			case "image_url", "input_image":
				var u string
				switch iu := p["image_url"].(type) {
				case string:
					u = iu
				case map[string]any:
					u, _ = iu["url"].(string)
				}
				if u == "" {
					u, _ = p["image_url"].(string)
				}
				if u == "" {
					continue
				}
				part := map[string]any{"type": "image", "image": u}
				if m := ccDataMime(u); m != "" {
					part["mimeType"] = m
				}
				parts = append(parts, part)
			}
		}
	}
	return parts
}

var ccDataURI = regexp.MustCompile(`^data:([^;,]+)[;,]`)

func ccDataMime(u string) string {
	if m := ccDataURI.FindStringSubmatch(u); len(m) == 2 {
		return m[1]
	}
	return ""
}

func ccFirstString(vs ...any) string {
	for _, v := range vs {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// ccParseArgs 把工具调用参数（JSON 字符串或对象）转成对象。
func ccParseArgs(v any) any {
	switch a := v.(type) {
	case string:
		var out any
		if json.Unmarshal([]byte(a), &out) == nil && out != nil {
			return out
		}
		return map[string]any{}
	case map[string]any:
		return a
	case nil:
		return map[string]any{}
	}
	return map[string]any{}
}

var ccEffortRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// ccFitEffort 把请求的档位收敛到模型支持的最近档位（同距取更高档）。
func ccFitEffort(want string, levels []string) string {
	if want == "ultra" && !ccContains(levels, want) {
		want = "max"
	}
	if len(levels) == 0 || ccContains(levels, want) {
		return want
	}
	at := -1
	for i, l := range ccEffortRank {
		if l == want {
			at = i
			break
		}
	}
	if at < 0 {
		return want
	}
	best := want
	dist := len(ccEffortRank)
	for _, l := range levels {
		i := -1
		for k, r := range ccEffortRank {
			if r == l {
				i = k
				break
			}
		}
		if i < 0 || l == "none" {
			continue
		}
		d := at - i
		if d < 0 {
			d = -d
		}
		if d < dist || (d == dist && i > at) {
			best, dist = l, d
		}
	}
	return best
}

func ccContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func ccIsGoogle(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "google/") || strings.Contains(m, "/gemini") || strings.HasPrefix(m, "gemini")
}

// ── Google 工具 schema ────────────────────────────────────────────────────

var ccDefRef = regexp.MustCompile(`^#/(?:\$defs|definitions)/[^/]+$`)

var (
	ccSchemaMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	ccSchemaOne  = []string{"items", "additionalProperties", "contains", "propertyNames", "not", "if", "then", "else", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems"}
	ccSchemaList = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
)

// ccGoogleSchema 把 Google 模型无法接受的嵌套 $ref 就地展开；根级 $defs /
// definitions 的直接子引用保持原样。实现对齐 magpie 插件的 googleSchema。
func ccGoogleSchema(schema any) any {
	b, err := json.Marshal(schema)
	if err != nil || !strings.Contains(string(b), `"$ref"`) {
		return schema
	}
	return ccSchemaWalk(schema, schema, nil, 0)
}

func ccSchemaWalk(root, s any, open []string, depth int) any {
	if arr, ok := s.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = ccSchemaWalk(root, v, open, depth)
		}
		return out
	}
	m, ok := s.(map[string]any)
	if !ok {
		return s
	}
	if ref, ok := m["$ref"].(string); ok && !(ccDefRef.MatchString(ref) && ccPointer(root, ref) != nil) {
		if t, ok := ccPointer(root, ref).(map[string]any); ok {
			notes := map[string]any{}
			for k, v := range m {
				if k != "$ref" {
					notes[k] = v
				}
			}
			if ccContains(open, ref) || depth >= 32 {
				if typ, ok := t["type"].(string); ok {
					notes["type"] = typ
				}
				return notes
			}
			merged := map[string]any{}
			for k, v := range t {
				merged[k] = v
			}
			for k, v := range notes {
				merged[k] = v
			}
			return ccSchemaWalk(root, merged, append(open, ref), depth+1)
		}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range ccSchemaMaps {
		if sub, ok := out[k].(map[string]any); ok {
			cp := make(map[string]any, len(sub))
			for n, v := range sub {
				cp[n] = ccSchemaWalk(root, v, open, depth)
			}
			out[k] = cp
		}
	}
	for _, k := range ccSchemaOne {
		if v, ok := out[k]; ok {
			if _, isMap := v.(map[string]any); isMap {
				out[k] = ccSchemaWalk(root, v, open, depth)
			}
		}
	}
	for _, k := range ccSchemaList {
		if arr, ok := out[k].([]any); ok {
			cp := make([]any, len(arr))
			for i, v := range arr {
				cp[i] = ccSchemaWalk(root, v, open, depth)
			}
			out[k] = cp
		}
	}
	return out
}

// ccPointer 解析 JSON Pointer（#/...）。
func ccPointer(root any, ref string) any {
	if ref == "#" {
		return root
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	cur := root
	for _, tok := range strings.Split(ref[2:], "/") {
		if u, err := url.PathUnescape(tok); err == nil {
			tok = u
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[tok]
		if !ok {
			return nil
		}
	}
	return cur
}

// ── 请求头 ────────────────────────────────────────────────────────────────

func ccHeaders(cred *CommandCodeCred) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", commandCodeCLIVersion)
	h.Set("x-cli-environment", "production")
	h.Set("x-project-slug", ccProjectSlug())
	h.Set("x-taste-learning", "false")
	h.Set("x-session-id", ccSessionID)
	h.Set("Authorization", "Bearer "+cred.APIKey)
	if cred.ZDR {
		h.Set("x-cmd-zdr", "1")
	}
	return h
}

func ccProjectSlug() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	name := path.Base(strings.ReplaceAll(cwd, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "root"
	}
	for _, r := range name {
		if !(r >= 0x20 && r <= 0x7e) && !(r >= 0xa0 && r <= 0xff) {
			return url.QueryEscape(name)
		}
	}
	return name
}

// ── 上游错误分类 ──────────────────────────────────────────────────────────

var ccQuotaWords = regexp.MustCompile(`(?i)quota|insufficient|balance|credit|billing|exceeded|rate.?limit|usage.?limit|limit.?reached|hit your .*limit|limit.{0,24}resets|too many requests|overloaded|余额|额度|欠费|限流|频率|套餐|用量|上限`)

// ccFailure 把 Command Code 的错误响应归一到 HTTP 状态与文案。
func ccFailure(status int, body []byte) (int, string) {
	msg := ccErrSnippet(body)
	if msg == "" {
		msg = http.StatusText(status)
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "MODEL_NOT_IN_PLAN:"):
		status = http.StatusForbidden
		msg = "Model not in plan: " + strings.TrimSpace(strings.TrimPrefix(msg, "MODEL_NOT_IN_PLAN:"))
	case strings.Contains(low, "model_not_in_plan"):
		status = http.StatusForbidden
	case strings.Contains(low, "premium_credits_exhausted") || strings.Contains(low, "insufficient credits"):
		status = http.StatusPaymentRequired
		msg = strings.TrimSpace(strings.TrimPrefix(msg, "PREMIUM_CREDITS_EXHAUSTED:"))
		if !ccQuotaWords.MatchString(msg) {
			msg = "out of credits: " + msg
		}
	case strings.Contains(msg, "RATE_LIMITED") || strings.Contains(low, "usage limit") || strings.Contains(low, "window_limit"):
		if status < 400 || status == http.StatusInternalServerError {
			status = http.StatusTooManyRequests
		}
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if len(msg) > 600 {
		msg = msg[:600]
	}
	return status, msg
}

// ccStreamError 解析 error 事件（字符串或 {message,statusCode,type,code}）。
type ccStreamError struct {
	Status  int
	Message string
	Type    string
}

func ccParseStreamError(raw json.RawMessage) ccStreamError {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		status, msg := ccFailure(http.StatusInternalServerError, []byte(s))
		return ccStreamError{Status: status, Message: msg}
	}
	var o struct {
		Message    string `json:"message"`
		StatusCode any    `json:"statusCode"`
		Type       string `json:"type"`
		Code       string `json:"code"`
	}
	_ = json.Unmarshal(raw, &o)
	status := 0
	switch n := o.StatusCode.(type) {
	case float64:
		status = int(n)
	case json.Number:
		if v, err := n.Int64(); err == nil {
			status = int(v)
		}
	}
	msg := o.Message
	if i := strings.IndexByte(msg, '{'); i >= 0 {
		if n, err := parseLeadingInt(msg[:i]); err == nil && n >= 400 {
			status = n
		}
		var j struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(msg[i:]), &j) == nil && j.Error.Message != "" {
			msg = j.Error.Message
		}
	}
	if msg == "" {
		msg = "Stream error"
	}
	st, m := ccFailure(statusOr(status, http.StatusInternalServerError), []byte(msg))
	out := ccStreamError{Status: st, Message: m, Type: o.Type}
	if o.Type == "cmd_zdr_no_providers" || o.Code == "CMD_ZDR_NO_PROVIDERS" {
		out.Type = "cmd_zdr_no_providers"
	}
	return out
}

func statusOr(v, def int) int {
	if v >= 400 && v <= 599 {
		return v
	}
	return def
}

func parseLeadingInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// ── 回程事件流 ────────────────────────────────────────────────────────────

type ccEvent struct {
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Input            json.RawMessage `json:"input"`
	Args             json.RawMessage `json:"args"`
	ProviderExecuted bool            `json:"providerExecuted"`
	CacheWriteTokens int64           `json:"cacheWriteTokens"`
	FinishReason     string          `json:"finishReason"`
	TotalUsage       *ccUsage        `json:"totalUsage"`
	Error            json.RawMessage `json:"error"`
}

type ccUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	InputTokenDetails struct {
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"inputTokenDetails"`
	OutputTokenDetails struct {
		ReasoningTokens int64 `json:"reasoningTokens"`
	} `json:"outputTokenDetails"`
}

const ccCutShortNote = "(The reply was cut short: Command Code closed the stream before it finished. Ask again to go on.)"

// ccAttempt 处理一个 Command Code 候选的完整生命周期。
func (s *Server) ccAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *requestTiming, sm *usageSample) error {
	defer s.store.Release(c.Model.ID)
	cred := ccOf(c.Provider)
	if cred == nil || strings.TrimSpace(cred.APIKey) == "" {
		err := fmt.Errorf("未配置 Command Code API Key")
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
				Message: "构造 Command Code 出站请求失败", Detail: err.Error()})
			return err
		}
	}
	body, err := ccBuildBody(src, c.Model.Name)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindBadRequest, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "构造 Command Code 出站请求失败", Detail: err.Error()})
		return err
	}
	cli, err := s.upstreamClient(c.Provider, 0)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "上游代理配置非法", Detail: err.Error()})
		return err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.ccAPI()+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "构造 Command Code 出站请求失败", Detail: err.Error()})
		return err
	}
	req.Header = ccHeaders(cred)
	resp, err := cli.Do(req)
	if err != nil {
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "连接 Command Code 失败（网络错误或超时）", Detail: err.Error()})
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status, msg := ccFailure(resp.StatusCode, eb)
		if status == http.StatusTooManyRequests {
			if until, ok := s.store.MuteOn429(c.Model.ID, rateLimitMute, "Command Code 限流/用量窗口已满"); ok {
				s.logFail(LogEntry{Level: "warn", Kind: kindMuted, Status: status, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: fmt.Sprintf("Command Code 返回 429，已自动静音至 %s", until.Local().Format("01-02 15:04")),
					Detail:  string(eb)})
				return fmt.Errorf("上游返回 %d: %s", status, msg)
			}
		}
		s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: status, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("Command Code 上游返回 HTTP %d", status), Detail: string(eb)})
		return fmt.Errorf("上游返回 %d: %s", status, msg)
	}
	return s.ccRelay(w, r, c, &firstByteReader{r: resp.Body, t: tm}, keyID, wantStream, responsesMode, tm, sm)
}

// ccRelay 把 /alpha/generate 的 NDJSON 事件流翻译成客户端协议。
func (s *Server) ccRelay(w http.ResponseWriter, r *http.Request, c Candidate, body io.Reader, keyID string, wantStream, responsesMode bool, tm *requestTiming, sm *usageSample) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 8<<20)

	id := "chatcmpl-" + wbMessageID()
	created := time.Now().Unix()
	model := c.Model.Name

	var rs *wbRespStream
	var agg *wbAgg
	started := false
	said := false  // 已经产生过任何内容/思考/工具调用
	wrote := false // 已经产生过正文（cut 时决定是否补空行）
	tools := 0
	var cacheWrite int64
	firstRaw := ""

	start := func() {
		if started {
			return
		}
		started = true
		if !wantStream {
			agg = &wbAgg{}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if responsesMode {
			rs = newWBRespStream(w, model)
			rs.begin()
			return
		}
		// chat 流：先发 role 分片
		writeCCChunk(flushWriter{w}, ccChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ccChoice{{Index: 0, Delta: ccDelta{Role: "assistant", Content: ""}, FinishReason: nil}},
		})
	}

	emit := func(delta ccDelta, finish any, usage map[string]any) {
		start()
		ch := ccChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ccChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if usage != nil {
			ch.Usage = usage
		}
		if !wantStream {
			b, _ := json.Marshal(ch)
			agg.feed(b)
			return
		}
		if responsesMode {
			b, _ := json.Marshal(ch)
			rs.feed(b)
			return
		}
		writeCCChunk(flushWriter{w}, ch)
	}

	finish := func(reason string, u ccUsage) {
		emit(ccDelta{}, reason, ccUsagePayload(u))
		if !wantStream {
			// 非流式：先把整段聚合完再提交 200，中途失败仍可换候选
			var out []byte
			var used Usage
			if responsesMode {
				out, used = agg.responsesResult(model)
			} else {
				out, used = agg.result(model)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
			if sm != nil {
				*sm = tm.sample(used, false)
			}
			return
		}
		if responsesMode {
			rs.finish(nil)
			return
		}
		fmt.Fprint(flushWriter{w}, "data: [DONE]\n\n")
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev ccEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			if !started && firstRaw == "" {
				firstRaw = line
			}
			continue
		}
		if ev.Type == "" && len(ev.Error) > 0 {
			ev.Type = "error" // 有的错误直接回 {"error":{...}} 信封
		}
		switch ev.Type {
		case "text-delta":
			if ev.Text == "" {
				continue
			}
			said, wrote = true, true
			emit(ccDelta{Content: ev.Text}, nil, nil)
		case "reasoning-delta":
			if ev.Text == "" {
				continue
			}
			said = true
			emit(ccDelta{ReasoningContent: ev.Text}, nil, nil)
		case "tool-call":
			if ev.ProviderExecuted {
				continue
			}
			input := ev.Input
			if len(input) == 0 || string(input) == "null" {
				input = ev.Args
			}
			args := "{}"
			if len(input) > 0 {
				var rawArgs any
				if json.Unmarshal(input, &rawArgs) == nil {
					if b, err := json.Marshal(ccParseArgs(rawArgs)); err == nil {
						args = string(b)
					}
				}
			}
			callID := ev.ToolCallID
			if callID == "" {
				callID = "call_" + wbMessageID()
			}
			call := ccDeltaToolCall{Index: tools, ID: callID, Type: "function"}
			call.Function.Name = ev.ToolName
			call.Function.Arguments = args
			tools++
			said = true
			emit(ccDelta{ToolCalls: []ccDeltaToolCall{call}}, nil, nil)
		case "cache-write-tokens":
			if ev.CacheWriteTokens > cacheWrite {
				cacheWrite = ev.CacheWriteTokens
			}
		case "finish":
			u := ccUsage{}
			if ev.TotalUsage != nil {
				u = *ev.TotalUsage
			}
			if cacheWrite > u.InputTokenDetails.CacheWriteTokens {
				u.InputTokenDetails.CacheWriteTokens = cacheWrite
			}
			reason := ccFinishReason(ev.FinishReason, tools)
			finish(reason, u)
			if sm != nil {
				*sm = tm.sample(ccUsageOf(u), wantStream)
			}
			return nil
		case "abort":
			finish("stop", ccUsage{})
			if sm != nil {
				*sm = tm.sample(Usage{}, wantStream)
			}
			return nil
		case "error":
			se := ccParseStreamError(ev.Error)
			if !started {
				s.logFail(LogEntry{Level: "error", Kind: kindUpstream, Status: se.Status, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "Command Code 事件流返回错误", Detail: se.Message})
				return fmt.Errorf("上游返回 %d: %s", se.Status, se.Message)
			}
			if !wantStream {
				return fmt.Errorf("上游返回 %d: %s", se.Status, se.Message)
			}
			if responsesMode {
				rs.finish(fmt.Errorf("%s", se.Message))
				return nil
			}
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": se.Message, "code": se.Status}})
			fmt.Fprintf(flushWriter{w}, "data: %s\n\n", b)
			return nil
		}
	}

	if err := sc.Err(); err != nil {
		if !started {
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Command Code 响应中断", Detail: err.Error()})
			return fmt.Errorf("上游响应中断: %v", err)
		}
		s.logFail(LogEntry{Level: "warn", Kind: kindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "Command Code 流式响应中断（已写出部分内容）", Detail: err.Error()})
		if responsesMode {
			rs.finish(err)
		}
		return nil
	}

	// 没有 finish 行：上游把流截断了。已经写出内容时补一句说明并收尾；
	// 什么都还没写时返回错误，让候选循环换下一家。
	errCut := fmt.Errorf("the reply ended before it was complete")
	if !said || !wantStream {
		if !started {
			detail := errCut.Error()
			if firstRaw != "" {
				detail = "上游返回非事件流内容: " + truncateRunes(firstRaw, 200)
			}
			s.logFail(LogEntry{Level: "error", Kind: kindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Command Code 响应在完成前结束", Detail: detail})
			if firstRaw != "" {
				return fmt.Errorf("%s", detail)
			}
		}
		return errCut
	}
	note := ccCutShortNote
	if wrote {
		note = "\n\n" + note
	}
	emit(ccDelta{Content: note}, nil, nil)
	reason := "stop"
	if tools > 0 {
		reason = "tool_calls"
	}
	finish(reason, ccUsage{})
	if sm != nil {
		*sm = tm.sample(Usage{}, true)
	}
	return nil
}

func writeCCChunk(w io.Writer, ch ccChunk) {
	b, err := json.Marshal(ch)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func ccFinishReason(reason string, tools int) string {
	switch reason {
	case "tool-calls", "tool_calls":
		return "tool_calls"
	case "length":
		return "length"
	case "content-filter", "content_filter":
		return "content_filter"
	}
	if tools > 0 {
		return "tool_calls"
	}
	return "stop"
}

func ccUsagePayload(u ccUsage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      u.InputTokenDetails.CacheReadTokens,
			"cache_write_tokens": u.InputTokenDetails.CacheWriteTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": u.OutputTokenDetails.ReasoningTokens,
		},
	}
}

func ccUsageOf(u ccUsage) Usage {
	return Usage{Prompt: u.InputTokens, Completion: u.OutputTokens, Cached: u.InputTokenDetails.CacheReadTokens}
}
