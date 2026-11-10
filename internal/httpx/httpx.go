// Package httpx 提供网关统一的 JSON 响应与请求解码小工具。
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
)

// WriteErr 输出统一形状的 JSON 错误。
func WriteErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "llmrouter_error"},
	})
}

// WriteJSON 输出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Decode 解码请求体（上限 1MB），失败时已写出 400。
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		WriteErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}
