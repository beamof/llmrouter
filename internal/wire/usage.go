package wire

import (
	"bytes"
	"encoding/json"

	"llmrouter/internal/domain"
)

// usageDetail usage 的分项对象。CachedTokens 用指针区分「字段缺失」与「显式 0」：
// 部分上游会同时在 completion_tokens_details 里返回 cached_tokens:0，不能拿它当
// 提示缓存（真正的值在 prompt_tokens_details / input_tokens_details）。
type usageDetail struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

// usagePayload 一次调用的 usage 对象，兼容 chat completions 与 responses 两套字段名。
type usagePayload struct {
	PromptTokens         int64        `json:"prompt_tokens"`
	CompletionTokens     int64        `json:"completion_tokens"`
	PromptTokensDetails  *usageDetail `json:"prompt_tokens_details"`
	InputTokens          int64        `json:"input_tokens"`
	OutputTokens         int64        `json:"output_tokens"`
	InputTokensDetails   *usageDetail `json:"input_tokens_details"`
	PromptCacheHitTokens *int64       `json:"prompt_cache_hit_tokens"` // DeepSeek 风格
	CacheReadInputTokens *int64       `json:"cache_read_input_tokens"` // Anthropic 风格
	CachedTokens         *int64       `json:"cached_tokens"`           // 顶层兜底
}

// parseUsageObject 解析 usage JSON 对象为记账用量。缓存命中取第一个存在的
// prompt/input 分项，再依次回落 DeepSeek / Anthropic / 顶层字段。
func ParseUsage(raw []byte) domain.Usage {
	if len(raw) == 0 {
		return domain.Usage{}
	}
	var v usagePayload
	if json.Unmarshal(raw, &v) != nil {
		return domain.Usage{}
	}
	u := domain.Usage{Prompt: v.PromptTokens, Completion: v.CompletionTokens}
	if u.Prompt == 0 && u.Completion == 0 {
		u.Prompt, u.Completion = v.InputTokens, v.OutputTokens
	}
	switch {
	case v.PromptTokensDetails != nil && v.PromptTokensDetails.CachedTokens != nil:
		u.Cached = *v.PromptTokensDetails.CachedTokens
	case v.InputTokensDetails != nil && v.InputTokensDetails.CachedTokens != nil:
		u.Cached = *v.InputTokensDetails.CachedTokens
	case v.PromptCacheHitTokens != nil:
		u.Cached = *v.PromptCacheHitTokens
	case v.CacheReadInputTokens != nil:
		u.Cached = *v.CacheReadInputTokens
	case v.CachedTokens != nil:
		u.Cached = *v.CachedTokens
	}
	return u
}

// extractStreamUsage 从 SSE 尾部提取最后一个非空 usage 对象里的 token 数。
// 按 data: 行逆序解析，避免正文里出现的 "usage" 文本被误当用量；单行被尾部
// 8KB 截断时退回花括号配对，保证 responses 的大事件也能取到 usage。
func ExtractStreamUsage(tail []byte) domain.Usage {
	for end := len(tail); end > 0; {
		i := bytes.LastIndexByte(tail[:end], '\n')
		line := tail[i+1 : end]
		if i < 0 { // tail 从某行中间开始：这一段仍尝试解析，失败会自然跳过
			line = tail[:end]
			end = 0
		} else {
			end = i
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		d, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			// tail 只保留最后 8KB，超大的 response.completed 事件可能连 data: 前缀
			// 都被截掉。对残余片段仍做一次 usage 对象配对（正文里的 "usage" 文本
			// 因为没有 prompt/completion 字段，解析后通常是全 0，不影响正常流）。
			if obj := lastUsageObject(line); obj != nil {
				return ParseUsage(obj)
			}
			continue
		}
		d = bytes.TrimSpace(d)
		if len(d) == 0 || bytes.Equal(d, []byte("[DONE]")) {
			continue
		}
		if obj := usageObjectInPayload(d); obj != nil {
			return ParseUsage(obj)
		}
	}
	return domain.Usage{}
}

// usageObjectInPayload 从一条 SSE data 载荷取 usage 对象：chat completions 在顶层，
// responses 事件在 response.usage。载荷是合法 JSON 但 usage 为 null/缺失时返回 nil，
// 继续向前找真正带用量的那一帧。
func usageObjectInPayload(d []byte) []byte {
	var top struct {
		Usage    json.RawMessage `json:"usage"`
		Response *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(d, &top) == nil {
		if !isNullOrEmptyJSON(top.Usage) {
			return top.Usage
		}
		if top.Response != nil && !isNullOrEmptyJSON(top.Response.Usage) {
			return top.Response.Usage
		}
		return nil
	}
	return lastUsageObject(d) // 行被 8KB 尾部截断
}

func isNullOrEmptyJSON(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("{}"))
}

// lastUsageObject 取 tail 中最后一个 "usage" 键对应的完整 JSON 对象；
// 找不到、值为 null 或对象被截断时返回 nil。
func lastUsageObject(tail []byte) []byte {
	i := bytes.LastIndex(tail, []byte(`"usage"`))
	if i < 0 {
		return nil
	}
	j := i + len(`"usage"`)
	for j < len(tail) && (tail[j] == ' ' || tail[j] == '\t' || tail[j] == '\r' || tail[j] == '\n' || tail[j] == ':') {
		j++
	}
	if j >= len(tail) || tail[j] != '{' {
		return nil // null 或残缺
	}
	depth := 0
	inStr, esc := false, false
	for k := j; k < len(tail); k++ {
		c := tail[k]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return tail[j : k+1]
			}
		}
	}
	return nil
}

// UsageOf 解析 usage 对象为记账用量（兼容 chat/responses 两套字段名及缓存分项）
func UsageOf(raw json.RawMessage) domain.Usage { return ParseUsage(raw) }
