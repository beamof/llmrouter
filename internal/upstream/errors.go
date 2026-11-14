package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// StatusError 上游返回的非成功状态码，携带原始响应头与响应体。
// 直连模型请求失败时按原样透传给客户端；路由组请求仍会换下一个候选。
type StatusError struct {
	Status int
	Header http.Header
	Body   []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("上游返回 HTTP %d: %s", e.Status, ErrSnippet(e.Body))
}

// ErrSnippet 提取上游错误信息，没有则截断原始响应
func ErrSnippet(b []byte) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"` // WorkBuddy 等私有信封
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(b, &v) == nil {
		switch {
		case v.Error != nil && v.Error.Message != "":
			return v.Error.Message
		case v.Message != "":
			return v.Message
		case v.Msg != "":
			return v.Msg
		}
	}
	s := string(b)
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80]) + "…"
	}
	return s
}
