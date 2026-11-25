package geminiapp

// 请求归一化与提示词渲染。两种接入协议（chat / responses）都先折成同一份
// canonicalRequest，再渲染成单条用户文本发往上游：系统指令、工具协议说明、
// 结构化输出约束放在 [System Instructions] 里，完整对话放进 [Conversation]。
// 网关不做历史压缩，每轮回放全部历史。提示词文本对齐 gemini-app2api。

import (
	"fmt"
	"maps"
	"strings"
)

// promptMaxBytes 渲染后提示词的字节上限
const promptMaxBytes = 4 << 20

// responseFormat 结构化输出约束（chat 的 response_format / responses 的 text.format）
type responseFormat struct {
	kind        string // json_object | json_schema
	name        string
	description string
	strict      bool
	hasStrict   bool
	schema      map[string]any
}

// imageInput 一张已解码校验的用户图片
type imageInput struct {
	mime string
	data []byte
}

// nsIdentity Responses 命名空间工具的身份（内部别名 → 命名空间 + 原始名）
type nsIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// canonCall 历史里的一次工具调用
type canonCall struct {
	ID, Name, Arguments string
}

// canonCallJSON 历史调用在提示词里的形态（实参渲染成解析后的 JSON 值）
type canonCallJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

// toolResultJSON 工具结果在提示词里的形态（JSON 引号防止调用方文本逃出分隔符）
type toolResultJSON struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// canonMessage 归一化后的一条消息：图片已换成占位标记，工具调用已拆出
type canonMessage struct {
	role       string // system | developer | user | assistant | tool
	content    string
	toolCalls  []canonCall
	toolCallID string
}

// canonicalRequest 归一化后的请求
type canonicalRequest struct {
	model          string
	thinkingLevel  int
	messages       []canonMessage
	policy         *toolPolicy
	identities     map[string]nsIdentity
	parallelCalls  bool
	responseFormat *responseFormat
	images         []imageInput
	prompt         string
	promptChars    int
}

// normalizeChat 把 chat 形态的请求体归一化。identities 由 responses 入口给出
// （命名空间工具的别名表），chat 入口为 nil。未知字段一律忽略。
func normalizeChat(body map[string]any, identities map[string]nsIdentity, preferNestedEffort bool) (*canonicalRequest, error) {
	req := &canonicalRequest{parallelCalls: true, thinkingLevel: 2, identities: identities}
	if ns, _ := body["native_search"].(bool); ns {
		return nil, badRequest("UNSUPPORTED_NATIVE_SEARCH", "Gemini App 上游暂不支持 native_search")
	}
	name, _ := body["model"].(string)
	model, ok := canonicalModel(name)
	if !ok {
		return nil, badRequest("UNSUPPORTED_MODEL", fmt.Sprintf("Gemini App 不支持模型 %q（可用：%s）", name, strings.Join(nativeModels, " / ")))
	}
	req.model = model
	if err := req.setThinking(body, preferNestedEffort); err != nil {
		return nil, err
	}
	if pc, ok := body["parallel_tool_calls"].(bool); ok {
		req.parallelCalls = pc
	}
	var err error
	if req.responseFormat, err = parseResponseFormat(body); err != nil {
		return nil, err
	}
	tools, _ := body["tools"].([]any)
	req.policy = compileToolPolicy(annotateTools(tools), body["tool_choice"])
	for name, id := range identities { // 命名空间工具：记下原始身份，对外输出时还原
		if decl := req.policy.registry[name]; decl != nil {
			decl.namespace, decl.origName = id.Namespace, id.Name
		}
	}

	rawMsgs, _ := body["messages"].([]any)
	if len(rawMsgs) == 0 {
		return nil, badRequest("MESSAGES_REQUIRED", "请求缺少 messages")
	}
	for _, raw := range rawMsgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "function": // 旧版 chat 的工具结果角色
			role = "tool"
		case "system", "developer", "user", "assistant", "tool":
		default:
			role = "user"
		}
		msg := canonMessage{role: role}
		if role == "tool" {
			msg.toolCallID, _ = m["tool_call_id"].(string)
			if msg.toolCallID == "" {
				msg.toolCallID, _ = m["name"].(string)
			}
		}
		if msg.content, err = messageText(m["content"], role, &req.images); err != nil {
			return nil, err
		}
		if role == "assistant" {
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					if fn == nil {
						continue
					}
					name, _ := fn["name"].(string)
					id, _ := call["id"].(string)
					msg.toolCalls = append(msg.toolCalls, canonCall{ID: id, Name: name, Arguments: argsText(fn["arguments"])})
				}
			}
		}
		req.messages = append(req.messages, msg)
	}
	if err := req.buildPrompt(); err != nil {
		return nil, err
	}
	return req, nil
}

// setThinking 思考档位：reasoning_effort / reasoning.effort 映射，或显式的
// thinking_level。chat 入口根字段优先，responses 入口嵌套字段优先。
func (req *canonicalRequest) setThinking(body map[string]any, preferNested bool) error {
	efforts := []any{body["reasoning_effort"]}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if preferNested {
			efforts = append([]any{r["effort"]}, efforts...)
		} else {
			efforts = append(efforts, r["effort"])
		}
	}
	for _, e := range efforts {
		if s, ok := e.(string); ok {
			if lvl, ok := effortLevels[strings.ToLower(strings.TrimSpace(s))]; ok {
				req.thinkingLevel = lvl
				break
			}
		}
	}
	if tl, ok := body["thinking_level"].(float64); ok {
		switch tl {
		case 3:
			return badRequest("NATIVE_DEEP_THINK_UNSUPPORTED", "thinking_level=3 是异步 Deep Think，Gemini App 上游不支持（可用 0 / 1 / 2 / 4）")
		case 0, 1, 2, 4:
			req.thinkingLevel = int(tl)
		}
	}
	return nil
}

// parseResponseFormat 结构化输出约束：chat 的 response_format，或 responses 的 text.format
func parseResponseFormat(body map[string]any) (*responseFormat, error) {
	spec, _ := body["response_format"].(map[string]any)
	if text, ok := body["text"].(map[string]any); ok {
		if f, ok := text["format"].(map[string]any); ok {
			spec = f
		}
	}
	if spec == nil {
		return nil, nil
	}
	switch typ, _ := spec["type"].(string); typ {
	case "", "text":
		return nil, nil
	case "json_object":
		return &responseFormat{kind: "json_object"}, nil
	case "json_schema":
		if inner, ok := spec["json_schema"].(map[string]any); ok {
			spec = inner
		}
		schema, ok := spec["schema"].(map[string]any)
		if !ok {
			return nil, badRequest("INVALID_RESPONSE_FORMAT", "response_format 的 json_schema 缺少 schema 对象")
		}
		rf := &responseFormat{kind: "json_schema", schema: schema}
		rf.name, _ = spec["name"].(string)
		rf.description, _ = spec["description"].(string)
		if strict, ok := spec["strict"].(bool); ok {
			rf.strict, rf.hasStrict = strict, true
		}
		if !toolNameRe.MatchString(rf.name) {
			rf.name = "response"
		}
		return rf, nil
	}
	return nil, badRequest("INVALID_RESPONSE_FORMAT", "不支持的 response_format 类型")
}

// argsText 工具实参统一成 JSON 文本（对象形态直接序列化）
func argsText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return mustCompactJSON(v)
}

// messageText 消息 content → 纯文本；图片部件换成占位标记并收集到 imgs。
func messageText(content any, role string, imgs *[]imageInput) (string, error) {
	switch c := content.(type) {
	case nil:
		return "", nil
	case string:
		return c, nil
	case []any:
		var b strings.Builder
		for _, raw := range c {
			p, _ := raw.(map[string]any)
			if p == nil {
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "text", "input_text", "output_text":
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			case "image_url", "input_image":
				if role != "user" {
					return "", badRequest("UNSUPPORTED_IMAGE_ROLE", "图片只能出现在 user 消息里")
				}
				url := ""
				switch iu := p["image_url"].(type) {
				case string:
					url = iu
				case map[string]any:
					url, _ = iu["url"].(string)
				}
				if len(*imgs) >= maxImages {
					return "", badRequest("IMAGE_COUNT_EXCEEDED", fmt.Sprintf("图片数量超过 %d 张上限", maxImages))
				}
				img, err := decodeImageDataURL(url, len(*imgs)+1)
				if err != nil {
					return "", err
				}
				*imgs = append(*imgs, img)
				if err := checkImageBudget(*imgs); err != nil { // 边解码边累计，避免先读完整个请求才拒绝
					return "", err
				}
				// 对话记录里按顺序留一个可读的占位标记
				fmt.Fprintf(&b, "\n[Attached user image %d: image-%d.%s]\n", len(*imgs), len(*imgs), imageExts[img.mime])
			}
		}
		return b.String(), nil
	}
	return "", nil
}

// schemaAnnotations 只起说明作用、不参与校验的 schema 关键字
var schemaAnnotations = map[string]bool{
	"$schema": true, "$id": true, "title": true, "default": true, "examples": true,
	"deprecated": true, "readOnly": true, "writeOnly": true, "format": true,
}

// stripSchemaAnnotations 递归抹掉说明性关键字。只沿 schema 承载键递归，
// properties / enum / const 里的同名字面量键不会被动到。
func stripSchemaAnnotations(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(obj))
	for k, child := range obj {
		if schemaAnnotations[k] {
			continue
		}
		switch k {
		case "properties", "$defs", "definitions", "patternProperties":
			if m, ok := child.(map[string]any); ok {
				c := make(map[string]any, len(m))
				for name, schema := range m {
					c[name] = stripSchemaAnnotations(schema)
				}
				out[k] = c
			} else {
				out[k] = child
			}
		case "items", "additionalProperties", "additionalItems", "not", "if", "then", "else":
			if arr, ok := child.([]any); ok {
				out[k] = stripSchemaAnnotationsList(arr)
			} else {
				out[k] = stripSchemaAnnotations(child)
			}
		case "anyOf", "oneOf", "allOf", "prefixItems":
			if arr, ok := child.([]any); ok {
				out[k] = stripSchemaAnnotationsList(arr)
			} else {
				out[k] = child
			}
		default:
			out[k] = child
		}
	}
	return out
}

func stripSchemaAnnotationsList(arr []any) []any {
	out := make([]any, len(arr))
	for i, item := range arr {
		out[i] = stripSchemaAnnotations(item)
	}
	return out
}

// annotateTools 工具声明里的参数 schema 抹掉说明性关键字（提示词更短，校验口径
// 一致）。chat 的嵌套形态与 responses 的扁平形态都认。
func annotateTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		copied := maps.Clone(t)
		if fn, ok := t["function"].(map[string]any); ok {
			inner := maps.Clone(fn)
			if _, has := inner["parameters"]; has {
				inner["parameters"] = stripSchemaAnnotations(inner["parameters"])
			}
			copied["function"] = inner
		} else if _, has := copied["parameters"]; has {
			copied["parameters"] = stripSchemaAnnotations(copied["parameters"])
		}
		out = append(out, copied)
	}
	return out
}

// buildStructuredOutputInstruction 结构化输出的提示词约束
func buildStructuredOutputInstruction(rf *responseFormat) string {
	if rf == nil {
		return ""
	}
	if rf.kind == "json_object" {
		return strings.Join([]string{
			"# Output Format Constraint (response_format: json_object)",
			"When providing your final assistant answer (not emitting a tool_call fence), output ONLY a single valid UTF-8 JSON object.",
			"Do not include surrounding prose, explanations, or markdown code fences.",
		}, "\n")
	}
	lines := []string{"# Output Format Constraint (response_format: json_schema)", "Schema name: " + rf.name}
	if rf.description != "" {
		lines = append(lines, "Description: "+rf.description)
	}
	if rf.hasStrict {
		lines = append(lines, "Strict: "+fmt.Sprintf("%t", rf.strict))
	}
	lines = append(lines,
		"When providing your final assistant answer (not emitting a tool_call fence), output ONLY a single valid UTF-8 JSON value strictly conforming to this JSON Schema.",
		"Do not include surrounding prose, explanations, or markdown code fences:",
		mustCompactJSON(rf.schema),
	)
	return strings.Join(lines, "\n")
}

// buildPrompt 渲染发往上游的单条用户文本
func (req *canonicalRequest) buildPrompt() error {
	var systems, transcript []string
	for _, m := range req.messages {
		switch m.role {
		case "system", "developer":
			systems = append(systems, m.content)
		case "user":
			transcript = append(transcript, "[User]\n"+m.content)
		case "assistant":
			entry := "[Assistant]\n" + m.content
			if len(m.toolCalls) > 0 {
				// 历史调用渲染成解析后的 JSON 值（id / name / arguments）
				calls := make([]canonCallJSON, 0, len(m.toolCalls))
				for _, c := range m.toolCalls {
					calls = append(calls, canonCallJSON{ID: c.ID, Name: c.Name, Arguments: decodeArgsObject(c.Arguments)})
				}
				entry += "\n[Prior markdown tool_call fence requests; not execution proof]\n" + mustCompactJSON(calls)
			}
			transcript = append(transcript, entry)
		case "tool":
			transcript = append(transcript, "[Caller tool result]\n"+mustCompactJSON(toolResultJSON{m.toolCallID, m.content}))
		}
	}
	if len(transcript) == 0 {
		return badRequest("NON_SYSTEM_MESSAGE_REQUIRED", "请求缺少可转换的 messages")
	}
	// 工具协议说明（或关闭工具时的替代说明）
	instruction := noToolInstruction
	if req.policy.enabled() {
		instruction = buildToolInstruction(req.policy.promptTools(), req.policy.choice, req.policy.forced)
		if !req.parallelCalls {
			instruction += "\nRequest at most one tool call for this turn."
		}
	}
	preamble := []string{strings.Join(systems, "\n\n"), instruction, buildStructuredOutputInstruction(req.responseFormat)}
	if len(req.identities) > 0 {
		preamble = append(preamble, "Namespace identities for internal tool names (including historical calls):\n"+mustCompactJSON(req.identities))
	}
	prompt := "[System Instructions]\n" + joinNonEmpty(preamble, "\n\n") + "\n\n[Conversation]\n" + strings.Join(transcript, "\n\n")
	if req.policy.enabled() {
		var names []string
		for _, name := range req.policy.order {
			names = append(names, name)
		}
		prompt += "\n\n[Tool Protocol Reminder]\nUpstream native/internal function-calling channels are disabled (`INVALID_ARGUMENT: function does not exist`). Never invoke tools through native/internal tool channels; to call `" +
			strings.Join(names, "`, `") +
			"`, write a literal plain-text Markdown ```tool_call fenced JSON block in your visible assistant response. Stop immediately after your tool_call block(s) and never emit [Caller tool result] yourself."
	}
	if len(prompt) > promptMaxBytes {
		return &turnError{Code: "PROMPT_TOO_LARGE", Status: 413, Message: "渲染后的提示词超过 4 MiB 上限"}
	}
	req.prompt = prompt
	req.promptChars = utf16Length(prompt)
	return nil
}

func joinNonEmpty(parts []string, sep string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// estimateUsage 上游不带 token 计数，按「规范提示词的 UTF-16 码元数 / 3.5 + 每张图
// 258」估输入、按「交付输出的 UTF-8 字节数 / 3.5」估输出。只是估算值，不是
// Google 的计费口径。
func (req *canonicalRequest) estimateUsage(outputBytes int) Usage {
	perToken := func(units int) int64 { return int64((2*units + 6) / 7) } // ceil(units / 3.5)
	return Usage{Prompt: perToken(req.promptChars) + int64(len(req.images))*258, Completion: perToken(outputBytes)}
}
