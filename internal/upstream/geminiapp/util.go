package geminiapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
)

// turnError 上游回合失败：Code 是稳定的网关错误码（写进响应体与日志），Status 是
// 回给客户端的 HTTP 状态，detail 是脱敏后的上游原文（只进日志）。
type turnError struct {
	Code    string
	Status  int
	Message string
	detail  string
}

func (e *turnError) Error() string {
	if e.detail != "" {
		return e.Message + "：" + e.detail
	}
	return e.Message
}

// badRequest 客户端可以修正的请求错误（4xx）
func badRequest(code, msg string) *turnError {
	return &turnError{Code: code, Status: 400, Message: msg}
}

// upstreamFailure 上游侧失败（5xx）
func upstreamFailure(code string, status int, msg string) *turnError {
	return &turnError{Code: code, Status: status, Message: msg}
}

// concat 拼接若干字节段
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// randomHex n 字节加密随机数的十六进制文本
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUID UUIDv4 文本（请求元数据里的客户端实例标识，每回合一个）
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func isSpaceTab(c byte) bool { return c == ' ' || c == '\t' }
