package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"llmrouter/internal/domain"
)

// wbAgg 把上游流式响应聚合为一个 chat.completion（上游不支持 stream:false，
// 非流式客户端由本网关代为聚合）
type Aggregator struct {
	lines     []byte
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolAcc
	finish    string
	usageRaw  json.RawMessage
}

type toolAcc struct {
	ID     string
	FnName string
	Args   strings.Builder
}

// Write 消费过滤后的 SSE 字节流（按行切出 data: 载荷）
func (a *Aggregator) Write(p []byte) (int, error) {
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
			a.Feed(bytes.TrimPrefix(d, []byte(" ")))
		}
	}
	return len(p), nil
}

// Feed 处理一条 data: 载荷
func (a *Aggregator) Feed(d []byte) {
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
			a.tools = map[int]*toolAcc{}
		}
		acc := a.tools[tc.Index]
		if acc == nil {
			acc = &toolAcc{}
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
func (a *Aggregator) Result(model string) ([]byte, domain.Usage) {
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
		"id":      "chatcmpl-" + MessageID(),
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
	return b, UsageOf(a.usageRaw)
}

// BodyStream 客户端是否要求流式（默认非流式）
func BodyStream(body []byte) bool {
	var t struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &t)
	return t.Stream
}
