package upstream

import (
	"encoding/json"
)

// ErrSnippet 提取上游错误信息，没有则截断原始响应
func ErrSnippet(b []byte) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil && v.Error != nil && v.Error.Message != "" {
		return v.Error.Message
	}
	s := string(b)
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80]) + "…"
	}
	return s
}
