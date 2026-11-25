package geminiapp

// 调用方自有工具的提示词协议（不是原生函数调用）。工具声明渲染进提示词，模型在
// 可见回复里用顶层 ```tool_call 围栏请求调用；一次调用只是「请求客户端执行」，
// 不是已执行的证据。引用块与普通代码块里的 tool_call 围栏只是文本；内联代码
// 引用也是文本；旧式的「正文后紧跟围栏」写法同样识别。

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// toolCall chat 形态的一次工具调用
type toolCall struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Function    toolCallFunction `json:"function"`
	nsName      string           // Responses 命名空间工具的原始名
	nsNamespace string           // 及其命名空间（两者非空时输出 namespace + name，不输出内部别名）
}

// toolCallFunction 工具调用的名称与实参（实参是 JSON 文本）
type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolDecl 一个已声明的工具
type toolDecl struct {
	canon      map[string]any // 提示词里序列化的声明（chat 形态）
	parameters map[string]any // 校验实参用的 schema
	namespace  string         // Responses 命名空间工具所属的命名空间
	origName   string         // 及其原始名（两者非空时 registry 里的键是内部别名）
}

// toolPolicy 已声明的工具与调用策略
type toolPolicy struct {
	registry map[string]*toolDecl
	order    []string // 声明顺序（内部名）
	choice   string   // auto | none | required | forced
	forced   string   // choice = forced 时指定的工具名
}

// enabled 本回合是否允许（并且能够）发起工具调用
func (p *toolPolicy) enabled() bool { return p.choice != "none" && len(p.registry) > 0 }

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// toolFailure 工具协议错误：残缺或不合法的调用不放行
func toolFailure(code, reason string) *turnError {
	msg := map[string]string{
		"MALFORMED_TOOL_CALL":           "模型输出的 tool_call 围栏格式不合法",
		"INVALID_TOOL_JSON":             "模型输出的 tool_call 内容不是合法 JSON",
		"INVALID_TOOL_CALL":             "模型输出的 tool_call 内容不合法",
		"TRUNCATED_TOOL_CALL":           "模型输出的 tool_call 围栏被截断",
		"TOOL_SIZE_LIMIT":               "模型输出的工具调用超过大小上限",
		"UNDECLARED_TOOL":               "模型请求了未声明的工具",
		"TOOL_CHOICE_MISMATCH":          "模型请求的工具与 tool_choice 指定的不一致",
		"INVALID_TOOL_ARGUMENTS":        "模型输出的工具实参不是 JSON 对象",
		"TOOL_ARGUMENT_SCHEMA_MISMATCH": "模型输出的工具实参不符合声明的 schema",
		"TOOL_CALL_NOT_ALLOWED":         "本回合禁止工具调用，但模型请求了工具",
		"RESPONSE_TOO_LARGE":            "模型输出超过大小上限",
	}[code]
	if msg == "" {
		msg = "工具协议校验失败"
	}
	if reason != "" {
		msg += "（" + reason + "）"
	}
	return &turnError{Code: code, Status: 502, Message: msg}
}

// compileToolPolicy 解析工具声明与 tool_choice。上游只承接 function 类型的工具，
// 其余类型（web_search、custom 等）与名称不合法的声明直接丢弃；指定了未声明
// 工具的 tool_choice 退回 auto。
func compileToolPolicy(tools []any, choice any) *toolPolicy {
	p := &toolPolicy{registry: map[string]*toolDecl{}, choice: "auto"}
	for _, raw := range tools {
		fn, ok := toolFunctionOf(raw)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if !toolNameRe.MatchString(name) {
			continue
		}
		if _, dup := p.registry[name]; dup {
			continue
		}
		parameters, _ := fn["parameters"].(map[string]any)
		if parameters == nil {
			parameters = map[string]any{"type": "object"}
		}
		p.registry[name] = &toolDecl{canon: map[string]any{"type": "function", "function": fn}, parameters: parameters}
		p.order = append(p.order, name)
	}
	switch c := choice.(type) {
	case string:
		if c == "none" || c == "required" {
			p.choice = c
		}
	case map[string]any:
		// {type:"function", function:{name}}（chat）或 {type:"function", name}（responses）
		if typ, _ := c["type"].(string); typ == "function" || typ == "" {
			name := ""
			if inner, ok := c["function"].(map[string]any); ok {
				name, _ = inner["name"].(string)
			}
			if name == "" {
				name, _ = c["name"].(string)
			}
			if _, ok := p.registry[name]; ok {
				p.choice, p.forced = "forced", name
			}
		}
	}
	return p
}

// toolFunctionOf 取工具声明里的 function 部分：chat 形态嵌套在 function 下，
// responses 形态是扁平对象。
func toolFunctionOf(raw any) (map[string]any, bool) {
	t, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	if typ, _ := t["type"].(string); typ != "function" {
		return nil, false
	}
	if fn, ok := t["function"].(map[string]any); ok {
		return fn, true
	}
	fn := map[string]any{}
	for k, v := range t {
		if k != "type" {
			fn[k] = v
		}
	}
	return fn, true
}

// promptTools 提示词里序列化的工具声明（按声明顺序）
func (p *toolPolicy) promptTools() []any {
	out := make([]any, 0, len(p.order))
	for _, name := range p.order {
		out = append(out, p.registry[name].canon)
	}
	return out
}

// validateToolCall 一次新产生的调用是否可以下发给客户端执行
func validateToolCall(name string, args any, policy *toolPolicy) error {
	if !policy.enabled() {
		return toolFailure("TOOL_CALL_NOT_ALLOWED", "")
	}
	decl, ok := policy.registry[name]
	if !ok {
		return toolFailure("UNDECLARED_TOOL", name)
	}
	if policy.choice == "forced" && name != policy.forced {
		return toolFailure("TOOL_CHOICE_MISMATCH", name)
	}
	obj, ok := args.(map[string]any)
	if !ok {
		return toolFailure("INVALID_TOOL_ARGUMENTS", name)
	}
	if !schemaMatches(obj, decl.parameters, decl.parameters, 0) {
		return toolFailure("TOOL_ARGUMENT_SCHEMA_MISMATCH", name)
	}
	return nil
}

// noToolInstruction 关闭工具（或没有声明工具）时的协议说明
const noToolInstruction = "Answer the latest user request now as the assistant. If caller tool results are present, use them to complete your answer; do not wait for another user message. Never invent tool outputs. Never emit transcript role markers such as [Caller tool result], [User], or [Assistant]. Tool calls are prohibited for this turn. Do not emit a tool_call fence. For literal examples use a json fence."

// buildToolInstruction 工具声明 + 调用协议说明
func buildToolInstruction(tools []any, choice, forced string) string {
	lines := []string{
		"# Available Local Tools (caller-owned prompt protocol, NOT native function calling)",
		mustCompactJSON(tools),
		"CRITICAL: Upstream native/internal function-calling channels are DISABLED on this backend and WILL return `INVALID_ARGUMENT: function does not exist` internally.",
		"NEVER invoke declared tools via native/internal tool-calling channels during reasoning or generation. Even if an internal tool channel reports `function does not exist`, ignore that internal error—declared tools ARE available exclusively as plain UTF-8 Markdown text in your visible assistant reply.",
		"To REQUEST execution, emit a top-level fenced block in your visible assistant text on separate lines:",
		"```tool_call",
		`{"name":"declared_name","arguments":{}}`,
		"```",
		"Arguments must be a JSON object matching the declared schema. Emit one strict UTF-8 JSON object per tool_call block with properly escaped strings (for multiple calls, use separate tool_call blocks; no trailing commas or raw newlines in strings). Never invent tools or results.",
		"Only the caller executes tools. Wait for linked caller tool results before using results.",
		"After emitting your tool_call block(s), STOP your response immediately. Never emit, simulate, or echo [Caller tool result], [User], [Assistant], or any transcript role markers.",
		"A standalone top-level tool_call fence is reserved for actual requests; use json fences for literal examples. Inline references are text.",
	}
	switch {
	case forced != "":
		lines = append(lines, "You MUST request only "+forced+".")
	case choice == "required":
		lines = append(lines, "You MUST request at least one declared tool.")
	default:
		lines = append(lines, "Request tools only when needed.")
	}
	return strings.Join(lines, "\n")
}

// ── 围栏解析器 ──────────────────────────────────────────────────────────────

const (
	toolBytesLimit     = 256 << 10 // 单个工具声明 / 调用的实参上限
	maxToolCalls       = 32
	splitterBytesLimit = 8 << 20
	callerResultMarker = "[Caller tool result]"
)

// splitEvent 围栏解析器产出的一种增量：正文文本或一次已校验的工具调用
type splitEvent struct {
	isCall bool
	text   string
	call   toolCall
}

type splitMode int

const (
	splitText     splitMode = iota // 普通文本
	splitTool                      // tool_call 围栏体内
	splitOrdinary                  // 普通代码块 / 引用围栏内
)

// fenceSplitter 流式的 tool_call 围栏解析器。只有 JSON、工具名与 schema 全部校验
// 通过的调用才会产出 call 事件；preview 暴露的是待定的普通文本，绝不会是工具体。
// 至少解析出一个真实调用之后，模型臆造的未加引号的 [Caller tool result] 及其后的
// 内容一律被抑制，客户端只会收到真实的工具调用。
type fenceSplitter struct {
	policy *toolPolicy

	pending        string // 尚未以换行结束的文本
	scanned        int    // pending 里已扫描过的字节数
	seen           int    // 已产出的调用数
	closed         bool
	suppressed     bool
	bytes          int
	consumedOffset int // 已消费的字节总数

	mode        splitMode
	marker      byte
	fenceLen    int
	toolBody    strings.Builder
	toolBytes   int
	headerBytes int
	inlineTicks int
}

func newFenceSplitter(policy *toolPolicy) *fenceSplitter {
	return &fenceSplitter{policy: policy}
}

// push 喂入一段文本增量，返回其中已确定的正文与调用
func (s *fenceSplitter) push(text string) ([]splitEvent, error) {
	if s.closed {
		return nil, toolFailure("SPLITTER_CLOSED", "")
	}
	s.bytes += len(text)
	if s.bytes > splitterBytesLimit {
		return nil, toolFailure("RESPONSE_TOO_LARGE", "")
	}
	if s.suppressed {
		return nil, nil
	}
	s.pending += text
	return s.drain(false)
}

// flush 文本结束时调用：处理最后一行；工具体未闭合则报截断
func (s *fenceSplitter) flush() ([]splitEvent, error) {
	if s.closed {
		return nil, toolFailure("SPLITTER_CLOSED", "")
	}
	s.closed = true
	if s.suppressed {
		return nil, nil
	}
	return s.drain(true)
}

// preview 可以先行下发的待定文本：可能是围栏开头的片段不暴露
func (s *fenceSplitter) preview() string {
	if s.suppressed || s.mode == splitTool {
		return ""
	}
	if s.mode == splitOrdinary {
		return s.pending
	}
	if strings.TrimSpace(s.pending) == "" && len(s.pending) <= 3 {
		return ""
	}
	safe := s.pending
	if tick := strings.IndexByte(safe, '`'); tick >= 0 {
		if tick <= 3 && isAllSpaces(safe[:tick]) {
			return "" // 行首（最多 3 个空格）之后是反引号：可能是围栏开头
		}
		safe = safe[:tick]
	}
	if s.seen > 0 {
		if cut := callerResultPreviewCut(safe); cut >= 0 {
			safe = safe[:cut]
		}
	}
	return safe
}

// state 当前解析状态
func (s *fenceSplitter) state() splitMode { return s.mode }

// callerResultPreviewCut 返回完整或尾部残缺的 CALLER_RESULT_MARKER 之前的安全长度；
// 没有则返回 -1。标记里只有下标 0 是 '['，尾部残缺的前缀必然从最后一个 '[' 开始。
func callerResultPreviewCut(str string) int {
	if full := strings.Index(str, callerResultMarker); full >= 0 {
		return full
	}
	lastBracket := strings.LastIndexByte(str, '[')
	if lastBracket >= 0 && strings.HasPrefix(callerResultMarker, str[lastBracket:]) {
		return lastBracket
	}
	return -1
}

// drain 逐行处理 pending；final 时把未换行的尾巴也当作一行
func (s *fenceSplitter) drain(final bool) ([]splitEvent, error) {
	events := make([]splitEvent, 0, 2)
	consumed, from := 0, s.scanned
	for {
		rel := strings.IndexByte(s.pending[from:], '\n')
		if rel < 0 {
			break
		}
		end := from + rel
		if err := s.line(s.pending[consumed:end+1], false, &events); err != nil {
			return nil, err
		}
		consumed = end + 1
		from = consumed
		if s.suppressed {
			s.consumedOffset += consumed
			s.pending, s.scanned = "", 0
			return events, nil
		}
	}
	if consumed > 0 {
		s.consumedOffset += consumed
		s.pending = s.pending[consumed:]
	}
	s.scanned = len(s.pending)
	if final && s.pending != "" {
		if err := s.line(s.pending, true, &events); err != nil {
			return nil, err
		}
		s.consumedOffset += len(s.pending)
		s.pending, s.scanned = "", 0
		if s.suppressed {
			return events, nil
		}
	}
	if s.mode == splitTool {
		if s.headerBytes+s.toolBytes+len(s.pending) > toolBytesLimit {
			return nil, toolFailure("TOOL_SIZE_LIMIT", "")
		}
		if final {
			return nil, toolFailure("TRUNCATED_TOOL_CALL", "")
		}
	}
	return events, nil
}

// line 处理一行（raw 含行尾换行；final 的尾行可能没有换行）
func (s *fenceSplitter) line(raw string, final bool, events *[]splitEvent) error {
	if s.suppressed {
		return nil
	}
	eol := ""
	if strings.HasSuffix(raw, "\r\n") {
		eol = "\r\n"
	} else if strings.HasSuffix(raw, "\n") {
		eol = "\n"
	}
	text := raw[:len(raw)-len(eol)]
	// 前缀匹配，避免在长行上回溯
	run, info, opening := splitOpening(text)
	if s.mode == splitTool {
		if opening && run[0] == '`' && len(run) >= s.fenceLen {
			// JSON / 工具名 / schema 校验（而不是围栏后的偶然排版）决定调用是否成立；
			// 校验通过后不再把后缀当成另一个请求扫描
			if err := s.emitCall(events); err != nil {
				return err
			}
			s.mode, s.inlineTicks = splitText, 0
			s.toolBody.Reset()
			s.toolBytes = 0
			if strings.TrimSpace(info) != "" {
				if idx := strings.Index(info, callerResultMarker); idx >= 0 {
					if idx > 0 {
						*events = append(*events, splitEvent{text: info[:idx]})
					}
					s.suppressed = true
					return nil
				}
				*events = append(*events, splitEvent{text: info + eol})
			} else if eol != "" {
				*events = append(*events, splitEvent{text: eol})
			}
			return nil
		}
		if opening && run[0] == '`' && strings.TrimSpace(info) != "" {
			return toolFailure("MALFORMED_TOOL_CALL", "closing_fence")
		}
		s.toolBody.WriteString(raw)
		s.toolBytes += len(raw)
		if s.toolBytes > toolBytesLimit+4 {
			return toolFailure("TOOL_SIZE_LIMIT", "")
		}
		return nil
	}
	if s.mode == splitOrdinary {
		*events = append(*events, splitEvent{text: raw})
		if opening && run[0] == s.marker && len(run) >= s.fenceLen && isBlank(info) {
			s.mode = splitText
		}
		return nil
	}
	if !opening || (run[0] == '`' && strings.ContainsRune(info, '`')) {
		s.inlineText(text, raw, eol, events)
		return nil
	}
	label := strings.TrimSpace(info)
	switch {
	case label == "tool_call" && run[0] == '`':
		if final && eol == "" {
			return toolFailure("TRUNCATED_TOOL_CALL", "")
		}
		s.mode, s.inlineTicks, s.fenceLen, s.headerBytes = splitTool, 0, len(run), len(raw)
		return nil
	case strings.HasPrefix(label, "tool_call"):
		return toolFailure("MALFORMED_TOOL_CALL", "opening_fence")
	case final && eol == "" && label != "" && strings.HasPrefix("tool_call", label):
		return toolFailure("TRUNCATED_TOOL_CALL", "")
	}
	s.mode, s.inlineTicks, s.marker, s.fenceLen = splitOrdinary, 0, run[0], len(run)
	*events = append(*events, splitEvent{text: raw})
	return nil
}

// inlineText 处理一行普通文本里的行内反引号与「正文后紧跟的 tool_call 围栏」
func (s *fenceSplitter) inlineText(text, raw, eol string, events *[]splitEvent) {
	if isQuotedLine(text) { // 缩进代码、引用块、列表项：整行是普通文本
		s.inlineTicks = 0
		if s.seen > 0 {
			if idx := strings.Index(text, callerResultMarker); idx >= 0 {
				if idx > 0 {
					*events = append(*events, splitEvent{text: text[:idx]})
				}
				s.suppressed = true
				return
			}
		}
		*events = append(*events, splitEvent{text: raw})
		return
	}
	if strings.TrimSpace(text) == "" {
		s.inlineTicks = 0
	}
	for i := 0; i < len(text); {
		if s.seen > 0 && s.inlineTicks == 0 && strings.HasPrefix(text[i:], callerResultMarker) {
			if i > 0 {
				*events = append(*events, splitEvent{text: text[:i]})
			}
			s.suppressed = true
			return
		}
		if text[i] == '\\' && s.inlineTicks == 0 {
			i += 2
			for i < len(text) && text[i] == '`' && text[i-1] == '`' {
				i++
			}
			continue
		}
		if text[i] != '`' {
			i++
			continue
		}
		end := i
		for end < len(text) && text[end] == '`' {
			end++
		}
		length := end - i
		// 历史上的成功响应可能在正文后面再接一个真正的围栏：仍要求标签精确且
		// 后面换行，并且不能落在行内代码里
		if s.inlineTicks == 0 && length >= 3 && eol != "" && isToolCallLabel(text[end:]) {
			if i > 0 {
				*events = append(*events, splitEvent{text: text[:i]})
			}
			s.mode, s.fenceLen, s.headerBytes = splitTool, length, len(text[i:])+len(eol)
			return
		}
		if s.inlineTicks == 0 {
			s.inlineTicks = length
		} else if s.inlineTicks == length {
			s.inlineTicks = 0
		}
		i = end
	}
	*events = append(*events, splitEvent{text: raw})
}

// emitCall 把围栏体内的 JSON 解析成调用并校验
func (s *fenceSplitter) emitCall(events *[]splitEvent) error {
	body := strings.TrimSpace(s.toolBody.String())
	if len(body) > toolBytesLimit {
		return toolFailure("TOOL_SIZE_LIMIT", "")
	}
	var parsed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return toolFailure("INVALID_TOOL_JSON", "")
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF { // 尾随内容
		return toolFailure("INVALID_TOOL_JSON", "")
	}
	if !toolNameRe.MatchString(parsed.Name) || len(parsed.Arguments) == 0 || parsed.Arguments[0] != '{' {
		return toolFailure("INVALID_TOOL_CALL", parsed.Name)
	}
	args, err := decodeJSONNumber(parsed.Arguments)
	if err != nil {
		return toolFailure("INVALID_TOOL_ARGUMENTS", parsed.Name)
	}
	if err := validateToolCall(parsed.Name, args, s.policy); err != nil {
		return err
	}
	if s.seen++; s.seen > maxToolCalls {
		return toolFailure("TOOL_SIZE_LIMIT", "")
	}
	compact, err := compactJSON(parsed.Arguments)
	if err != nil {
		return toolFailure("INVALID_TOOL_ARGUMENTS", parsed.Name)
	}
	decl := s.policy.registry[parsed.Name] // validateToolCall 已确认存在
	*events = append(*events, splitEvent{isCall: true, call: toolCall{
		ID: "call_" + randomHex(12), Type: "function",
		Function: toolCallFunction{Name: parsed.Name, Arguments: compact},
		nsName:   decl.origName, nsNamespace: decl.namespace,
	}})
	return nil
}

// splitOpening 行首的围栏开头（最多 3 个空格 + 3 个以上同种反引号 / 波浪号）
func splitOpening(text string) (run, info string, ok bool) {
	i := 0
	for i < len(text) && i < 3 && text[i] == ' ' {
		i++
	}
	if i >= len(text) || (text[i] != '`' && text[i] != '~') {
		return "", "", false
	}
	j := i
	for j < len(text) && text[j] == text[i] {
		j++
	}
	if j-i < 3 {
		return "", "", false
	}
	return text[i:j], text[j:], true
}

// isAllSpaces 字符串是否全为空格（空串也算）
func isAllSpaces(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return false
		}
	}
	return true
}

func isBlank(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSpaceTab(s[i]) {
			return false
		}
	}
	return true
}

// isToolCallLabel 围栏开头之后是否恰好是 tool_call 标签
func isToolCallLabel(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	return strings.HasPrefix(rest, "tool_call") && isBlank(rest[len("tool_call"):])
}

// isQuotedLine 缩进代码（4 个空格 / 制表符）、引用块或列表项的行
func isQuotedLine(text string) bool {
	if strings.HasPrefix(text, "    ") || strings.HasPrefix(text, "\t") {
		return true
	}
	i := 0
	for i < len(text) && i < 3 && text[i] == ' ' {
		i++
	}
	if i >= len(text) {
		return false
	}
	switch c := text[i]; c {
	case '>', '-', '+', '*':
		if c == '>' {
			return true
		}
		return i+1 < len(text) && isSpaceTab(text[i+1])
	default:
		j := i
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		return j > i && j+1 < len(text) && (text[j] == '.' || text[j] == ')') && isSpaceTab(text[j+1])
	}
}
