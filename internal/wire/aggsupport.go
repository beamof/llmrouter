package wire

import (
	"encoding/json"
	"strings"
)

// EffortArg 客户端显式给的思考等级：reasoning_effort / reasoningEffort /
// reasoning.effort 三种写法都认（Responses 协议用 reasoning.effort）。
func EffortArg(body map[string]any) (string, bool) {
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

// RequestEffort 客户端声明的思考等级；thinking.type=disabled 记为 "off"。
func RequestEffort(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return ""
	}
	if t, _ := m["thinking"].(map[string]any); t != nil {
		if s, _ := t["type"].(string); strings.EqualFold(strings.TrimSpace(s), "disabled") {
			return "off"
		}
	}
	if s, ok := EffortArg(m); ok {
		return s
	}
	return ""
}
