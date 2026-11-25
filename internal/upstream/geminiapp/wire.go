package geminiapp

// ProcessQuery 回程：响应帧解析与上游状态分类。

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxFrameBytes 单个响应帧的上限
const maxFrameBytes = 1 << 20

const (
	maxTurnFrames = 4096
	maxTurnWire   = 256 << 20
)

// turnFrame ClientGenerateResponse 里网关关心的字段。响应是「整文快照」：
// 每个带正文的帧都给出到目前为止的完整文本，而不是增量。
type turnFrame struct {
	text            string
	hasText         bool
	responseID      string
	completed       bool
	fallback        int // -1 = 缺失
	attribution     string
	downgradeReason int // -1 = 缺失
	resolvedMode    int // -1 = 缺失
	thinkingLevel   int // -1 = 缺失
	toolOps         bool
	refusal         bool
	thoughtBytes    int
}

var errInvalidResponse = &turnError{Code: "INVALID_NATIVE_RESPONSE", Status: 502, Message: "Gemini 上游返回了无法解析的响应帧"}

// decodeFrame 解析一条 ProcessQuery 响应消息。外层字段 1003 包着生成事件
// （字段 1 = ClientGenerateResponse）；其他消息（如续接句柄）返回 ok = false。
func decodeFrame(msg []byte) (tf turnFrame, ok bool, err error) {
	top, err := pbParse(msg)
	if err != nil {
		return tf, false, errInvalidResponse
	}
	wrapper, ok := pbGet(top, 1003)
	if !ok || wrapper.Wire != 2 {
		return tf, false, nil
	}
	env, err := pbParse(wrapper.Payload)
	if err != nil {
		return tf, false, errInvalidResponse
	}
	client, ok := pbGet(env, 1)
	if !ok || client.Wire != 2 {
		return tf, false, nil
	}
	fields, err := pbParse(client.Payload)
	if err != nil {
		return tf, false, errInvalidResponse
	}
	tf.fallback, tf.downgradeReason, tf.resolvedMode, tf.thinkingLevel = -1, -1, -1, -1
	if model, ok := pbGet(fields, 1); ok && model.Wire == 2 {
		mf, err := pbParse(model.Payload)
		if err != nil {
			return tf, false, errInvalidResponse
		}
		tf.responseID, _ = pbText(mf, 1)
		if display, ok := pbGet(mf, 2); ok && display.Wire == 2 {
			df, err := pbParse(display.Payload)
			if err != nil {
				return tf, false, errInvalidResponse
			}
			if text, ok := pbText(df, 1); ok {
				tf.text, tf.hasText = text, true
			}
		}
		// 思考内容不外发，只量字节数用于空闲判定
		if thoughts, ok := pbGet(mf, 12); ok && thoughts.Wire == 2 {
			if ts, err := pbParse(thoughts.Payload); err == nil {
				for _, f := range ts {
					if f.Wire == 2 && (f.Number == 1 || f.Number == 3) {
						tf.thoughtBytes += len(f.Payload)
					}
				}
			}
		}
		for _, f := range mf {
			if f.Number == 16 && f.Wire == 2 {
				tf.thoughtBytes += len(f.Payload)
			}
		}
	}
	if _, ok := pbGet(fields, 7); ok { // 原生工具操作：网关不承接
		tf.toolOps = true
	}
	if v, ok := pbUint(fields, 9); ok && v == 1 {
		tf.completed = true
	}
	if v, ok := pbUint(fields, 11); ok {
		tf.fallback = int(v)
	}
	if v, ok := pbUint(fields, 31); ok {
		tf.downgradeReason = int(v)
	}
	if v, ok := pbUint(fields, 41); ok {
		tf.resolvedMode = int(v)
	}
	if v, ok := pbUint(fields, 42); ok {
		tf.thinkingLevel = int(v)
	}
	if s, ok := pbText(fields, 30); ok {
		tf.attribution = s
	}
	_, r13 := pbGet(fields, 13)
	_, r17 := pbGet(fields, 17)
	tf.refusal = r13 || r17
	return tf, true, nil
}

// ── 上游状态 ────────────────────────────────────────────────────────────────

// rpcStatus 一次 Robin 调用结束时的上游状态。Grpc 为 -1 表示上游没给 grpc-status
// （成功响应的尾部有时不带）。
type rpcStatus struct {
	HTTP    int
	Grpc    int
	Message string // 已脱敏的 grpc-message
}

// readRPCStatus 合并响应头与尾部帧里的 gRPC 状态。出错时状态可能只出现在
// 响应头（trailers-only 响应），所以两处都看，尾部帧优先。
func readRPCStatus(resp *http.Response) rpcStatus {
	st := rpcStatus{HTTP: resp.StatusCode, Grpc: -1}
	merged := http.Header{}
	for k, vv := range resp.Header {
		merged[k] = append([]string(nil), vv...)
	}
	for k, vv := range resp.Trailer {
		merged[k] = append([]string(nil), vv...)
	}
	if raw := merged.Get("Grpc-Status"); raw != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 0 && n <= 16 {
			st.Grpc = n
		}
	}
	if raw := merged.Get("Grpc-Message"); raw != "" {
		st.Message = sanitizeUpstreamMessage(percentDecode(raw))
	}
	return st
}

// grpc 状态码
const (
	grpcDeadlineExceeded   = 4
	grpcPermissionDenied   = 7
	grpcResourceExhausted  = 8
	grpcFailedPrecondition = 9
	grpcUnavailable        = 14
	grpcUnauthenticated    = 16
)

// quotaDowngradeCode 已核实的降级原因值：哪些表示套餐额度问题（而不是单纯的
// 模型身份变化）。
func quotaDowngradeCode(reason int) string {
	switch reason {
	case 2, 3, 6:
		return "NATIVE_QUOTA_EXHAUSTED"
	case 8:
		return "NATIVE_QUOTA_INSUFFICIENT"
	}
	return ""
}

// statusFailure 把非成功状态折成回合错误；状态正常时返回 nil。
func statusFailure(st rpcStatus) *turnError {
	switch {
	case st.HTTP == http.StatusTooManyRequests || st.Grpc == grpcResourceExhausted:
		return &turnError{Code: "NATIVE_RATE_LIMITED", Status: 429,
			Message: "Gemini 上游返回 429（请求过于频繁）", detail: st.Message}
	case st.HTTP == http.StatusUnauthorized || st.Grpc == grpcUnauthenticated:
		return &turnError{Code: "NATIVE_AUTH_REJECTED", Status: 401,
			Message: "Gemini 账号鉴权被拒（access token 无效）", detail: st.Message}
	case st.HTTP == http.StatusForbidden || st.Grpc == grpcPermissionDenied:
		return &turnError{Code: "NATIVE_PERMISSION_OR_AUTH", Status: 403,
			Message: "Gemini 账号无权使用该模型（需要有效的 Gemini / Google AI 订阅）", detail: st.Message}
	case st.HTTP == http.StatusServiceUnavailable || st.Grpc == grpcUnavailable:
		return &turnError{Code: "NATIVE_UPSTREAM_UNAVAILABLE", Status: 503,
			Message: "Gemini 上游暂不可用（503）", detail: st.Message}
	case st.HTTP == http.StatusOK && st.Grpc == grpcFailedPrecondition:
		return &turnError{Code: "NATIVE_PRECONDITION_REQUIRED", Status: 412,
			Message: "Gemini 上游要求先满足前置条件（通常是账号尚未接受服务条款，见异常日志详情）", detail: st.Message}
	case st.HTTP == http.StatusGatewayTimeout || st.Grpc == grpcDeadlineExceeded:
		return &turnError{Code: "NATIVE_TURN_TIMEOUT", Status: 504,
			Message: "Gemini 上游处理超时", detail: st.Message}
	case st.HTTP != http.StatusOK:
		return &turnError{Code: "NATIVE_HTTP_REJECTED", Status: 502,
			Message: fmt.Sprintf("Gemini 上游返回 HTTP %d", st.HTTP), detail: st.Message}
	case st.Grpc > 0:
		return &turnError{Code: "NATIVE_TURN_FAILED", Status: 502,
			Message: fmt.Sprintf("Gemini 上游返回 gRPC 状态 %d", st.Grpc), detail: st.Message}
	}
	return nil
}

// unary 在 Robin 服务上执行一次一元调用，返回唯一一条响应消息。
func (a *Adapter) unary(ctx context.Context, cli *http.Client, token, path string, payload []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.robinOrigin()+path, bytes.NewReader(grpcFrame(payload)))
	if err != nil {
		return nil, err
	}
	setRPCHeaders(req.Header, token, timeout)
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Gemini 上游失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Gemini 上游响应失败: %w", err)
	}
	st := readRPCStatus(resp)
	if f := statusFailure(st); f != nil {
		return nil, f
	}
	rd := bufio.NewReader(bytes.NewReader(b))
	msg, err := grpcReadFrame(rd, 512<<10)
	if err != nil {
		return nil, &turnError{Code: "INVALID_NATIVE_RESPONSE", Status: 502, Message: "Gemini 上游响应不是合法的 gRPC 消息"}
	}
	if _, err := grpcReadFrame(rd, 512<<10); err != io.EOF {
		return nil, &turnError{Code: "INVALID_NATIVE_RESPONSE", Status: 502, Message: "Gemini 上游返回了多条响应消息"}
	}
	return msg, nil
}

// setRPCHeaders gRPC over HTTP/2 的请求头
func setRPCHeaders(h http.Header, token string, timeout time.Duration) {
	h.Set("Content-Type", "application/grpc")
	h.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	h.Set("TE", "trailers")
	h.Set("Grpc-Timeout", strconv.Itoa(int(timeout/time.Millisecond))+"m") // gRPC 超时单位 m = 毫秒
	h.Set("User-Agent", gatewayUserAgent)
	h.Set("Accept-Encoding", "identity")
}

var (
	reUpstreamURL   = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	reUpstreamEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reUpstreamCred  = regexp.MustCompile(`(?i)\b(?:ya29\.[A-Za-z0-9._-]+|GOCSPX-[A-Za-z0-9_-]+|Bearer\s+\S+)|1//[A-Za-z0-9._-]+`)
	reUpstreamIdent = regexp.MustCompile(`\b[A-Za-z0-9_-]{64,}\b`)
	reUpstreamCtrl  = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f]`)
)

// percentDecode 解开 grpc-message 的百分号编码（与 URI 编码兼容，'+' 不转空格）
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// sanitizeUpstreamMessage 脱敏后的上游报错原文：抹掉 URL、邮箱、凭证与长标识，
// 去掉控制字符并截断，避免把敏感信息写进异常日志。
func sanitizeUpstreamMessage(s string) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = reUpstreamURL.ReplaceAllString(s, "[URL 已隐去]")
	s = reUpstreamEmail.ReplaceAllString(s, "[邮箱已隐去]")
	s = reUpstreamCred.ReplaceAllString(s, "[凭证已隐去]")
	s = reUpstreamIdent.ReplaceAllString(s, "[标识已隐去]")
	s = reUpstreamCtrl.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 600 {
		s = string(r[:600]) + "…"
	}
	return s
}
