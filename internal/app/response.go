package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"llmrouter/internal/httpx"
	"llmrouter/internal/upstream"
)

func writeErr(w http.ResponseWriter, status int, msg string) { httpx.WriteErr(w, status, msg) }

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

// writeUpstreamError 把上游错误按原始状态码/响应头/响应体透传给客户端。
// 返回 false 表示不是可透传的类型化错误（网络错误、本地构造失败等），
// 调用方自行回退为网关 502。
func writeUpstreamError(w http.ResponseWriter, err error) bool {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return false
	}
	status := se.Status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway // 200 里包着的错误信封不能按 200 回
	}
	for k, vv := range se.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Content-Encoding":
			continue // 响应体可能被截断/解压，长度与编码头不能照抄
		}
		if upstream.SkipRespHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if len(se.Body) == 0 {
		writeErr(w, status, http.StatusText(status))
		return true
	}
	if w.Header().Get("Content-Type") == "" {
		if json.Valid(se.Body) {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	w.WriteHeader(status)
	w.Write(se.Body)
	return true
}
