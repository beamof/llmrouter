package geminiapp

// /v1/responses 请求 → chat 形态，随后与 chat 入口走同一条归一化与提示词路径。
// 网关不做有状态转换：previous_response_id / conversation / store 一律忽略，
// 客户端每轮带全量 input。回放的 reasoning 条目（密文或摘要）不参与提示词。

import (
	"fmt"
	"maps"
	"strings"
)

// toolNamer Responses 工具声明的归一器：扁平 function 直接包成 chat 形态；
// namespace 工具拍平，并用无冲突的内部别名（ns_0、ns_1…）命名，对外输出时再还原。
type toolNamer struct {
	tools      []any
	aliases    map[string]string // namespace + name → 内部别名
	identities map[string]nsIdentity
	reserved   map[string]bool
	next       int
}

func newToolNamer(body map[string]any) *toolNamer {
	n := &toolNamer{aliases: map[string]string{}, identities: map[string]nsIdentity{}, reserved: map[string]bool{}}
	type decl struct {
		tool, namespace, description string
		raw                          map[string]any
	}
	var decls []decl
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		if typ, _ := t["type"].(string); typ == "namespace" {
			ns, _ := t["name"].(string)
			desc, _ := t["description"].(string)
			children, _ := t["tools"].([]any)
			for _, child := range children {
				if c, ok := child.(map[string]any); ok {
					decls = append(decls, decl{tool: "function", namespace: ns, description: desc, raw: c})
				}
			}
			continue
		}
		decls = append(decls, decl{raw: t})
	}
	// 历史里的扁平调用名也保留：未声明的旧调用名不能恰好撞上现在生成的别名
	if items, ok := body["input"].([]any); ok {
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item == nil || item["namespace"] != nil {
				continue
			}
			switch item["type"] {
			case "function_call", "custom_tool_call":
				if name, ok := item["name"].(string); ok {
					n.reserved[name] = true
				}
			}
		}
	}
	for _, d := range decls {
		fn := d.raw
		if typ, _ := fn["type"].(string); typ != "" && typ != "function" {
			continue
		}
		name, _ := fn["name"].(string)
		if !toolNameRe.MatchString(name) {
			continue
		}
		mapped := maps.Clone(fn)
		delete(mapped, "type")
		if _, has := mapped["parameters"]; has {
			mapped["parameters"] = stripSchemaAnnotations(mapped["parameters"])
		}
		if d.namespace == "" {
			n.reserved[name] = true
		} else {
			alias := n.alias(d.namespace, name)
			desc, _ := fn["description"].(string)
			mapped["name"] = alias
			mapped["description"] = "Namespace tool " + mustCompactJSON(nsIdentity{d.namespace, name}) + ". " + d.description + "\n" + desc
		}
		n.tools = append(n.tools, map[string]any{"type": "function", "function": mapped})
	}
	return n
}

// alias 取（或生成）命名空间工具的内部别名
func (n *toolNamer) alias(namespace, name string) string {
	key := namespace + "\x00" + name
	if alias, ok := n.aliases[key]; ok {
		return alias
	}
	alias := ""
	for {
		alias = "ns_" + itoa(n.next)
		n.next++
		if !n.reserved[alias] {
			break
		}
	}
	n.reserved[alias] = true
	n.aliases[key] = alias
	n.identities[alias] = nsIdentity{Namespace: namespace, Name: name}
	return alias
}

// toolChoice tool_choice：responses 的 {type:"function", name} 包成 chat 形态
func (n *toolNamer) toolChoice(v any) any {
	c, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if typ, _ := c["type"].(string); typ == "function" || typ == "" {
		name, _ := c["name"].(string)
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return "auto"
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// responsesToChat /v1/responses 请求体 → chat 形态 + 命名空间别名表
func responsesToChat(body map[string]any) (map[string]any, map[string]nsIdentity, error) {
	out := map[string]any{}
	for _, k := range []string{"model", "stream", "reasoning", "reasoning_effort", "thinking_level", "parallel_tool_calls", "text", "native_search"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	namer := newToolNamer(body)
	out["tools"] = namer.tools
	if choice, ok := body["tool_choice"]; ok {
		out["tool_choice"] = namer.toolChoice(choice)
	}

	var msgs []any
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instr})
	}
	input := body["input"]
	if s, ok := input.(string); ok {
		input = []any{map[string]any{"role": "user", "content": s}}
	}
	items, _ := input.([]any)
	if len(items) == 0 {
		return nil, nil, badRequest("INPUT_REQUIRED", "/responses 请求缺少可用的 input")
	}
	toolImages := []any{} // 工具结果里的图片：顺延成紧随其后的一条 user 消息
	flushImages := func() {
		if len(toolImages) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": toolImages})
			toolImages = []any{}
		}
	}
	toolImageSeq := 0
	hasInput := false // 是否有可转换的输入条目（只有 instructions 不算）
	appendCall := func(call map[string]any) {
		flushImages()
		if n := len(msgs); n > 0 {
			if prev, ok := msgs[n-1].(map[string]any); ok && prev["role"] == "assistant" {
				calls, _ := prev["tool_calls"].([]any)
				prev["tool_calls"] = append(calls, call)
				return
			}
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		callID, _ := item["call_id"].(string)
		switch typ, _ := item["type"].(string); typ {
		case "reasoning", "item_reference", "compaction", "additional_tools", "configuration_update":
			// 回放的推理条目（密文 / 摘要）与有状态条目不参与提示词
		case "function_call", "custom_tool_call":
			hasInput = true
			if callID == "" {
				continue
			}
			name := ""
			if ns, _ := item["namespace"].(string); ns != "" && typ == "function_call" {
				name = namer.alias(ns, stringOf(item["name"]))
			} else {
				name, _ = item["name"].(string)
			}
			args := ""
			if typ == "function_call" {
				args, _ = item["arguments"].(string)
			} else {
				// 自由文本工具（custom）的入参包成 {input: 原文}，历史只读回放
				if in, ok := item["input"].(string); ok {
					args = mustCompactJSON(map[string]any{"input": in})
				}
			}
			appendCall(map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}})
		case "function_call_output", "custom_tool_call_output":
			hasInput = true
			if callID == "" {
				continue
			}
			var text strings.Builder
			output := item["output"]
			if wrap, ok := output.(map[string]any); ok {
				output = wrap["content"]
			}
			switch o := output.(type) {
			case string:
				text.WriteString(o)
			case []any:
				for _, rawPart := range o {
					p, _ := rawPart.(map[string]any)
					if p == nil {
						continue
					}
					switch p["type"] {
					case "input_text", "text", "output_text":
						if s, ok := p["text"].(string); ok {
							text.WriteString(s)
						}
					case "input_image":
						toolImageSeq++
						fmt.Fprintf(&text, "\n[Tool image %d for %s]\n", toolImageSeq, callID)
						toolImages = append(toolImages,
							map[string]any{"type": "text", "text": fmt.Sprintf("[Tool image %d for %s]:", toolImageSeq, callID)},
							p)
					}
				}
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": text.String()})
		case "input_text":
			hasInput = true
			flushImages()
			if s, ok := item["text"].(string); ok {
				msgs = append(msgs, map[string]any{"role": "user", "content": s})
			}
		case "input_image":
			hasInput = true
			flushImages()
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": item["image_url"]}}})
		case "", "message":
			hasInput = true
			flushImages()
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			content := item["content"]
			// 一个 Responses 回合里可能在并行调用之间穿插 assistant 的说明文字：
			// 合并到前一条 assistant 消息上，兄弟调用仍留在同一个工具批次里
			if n := len(msgs); n > 0 && role == "assistant" {
				if prev, ok := msgs[n-1].(map[string]any); ok && prev["role"] == "assistant" {
					if s, ok := content.(string); ok && s != "" {
						if before, _ := prev["content"].(string); before != "" {
							prev["content"] = before + "\n" + s
						} else {
							prev["content"] = s
						}
					}
					continue
				}
			}
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		}
	}
	flushImages()
	if !hasInput {
		return nil, nil, badRequest("INPUT_REQUIRED", "/responses 请求缺少可用的 input")
	}
	out["messages"] = msgs
	return out, namer.identities, nil
}
