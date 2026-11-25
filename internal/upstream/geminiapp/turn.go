package geminiapp

// 回合执行：读 ProcessQuery 的响应流、校验模型档位与归属、把整文快照折成增量
// 交给围栏解析器，再发布给回程交付层。
//
//	客户端\模式 | chat 流式 | chat 非流式 | responses 流式     | responses 非流式
//	处理方式    | 增量直写   | 聚合        | 渲染 responses 事件 | 聚合 response 对象
//
// 中断规则：流式下已交付「≥ 2048 字节正文或任一工具调用」之后上游断流 / 超时，
// 平稳收尾并标记 incomplete（否则客户端会丢弃已收到的回复并重发整段历史）；
// 不足这个门槛的残片不算可用，按失败处理，交还候选循环换下一个候选。

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"llmrouter/internal/upstream"
)

// prefixBytes 流式提交门槛：首段不足这个长度的正文先不提交响应头
const prefixBytes = 2048

// keepaliveInterval 流式已提交后，静默期间发 SSE 注释保活的间隔
const keepaliveInterval = 15 * time.Second

// turnRequest 一个上游回合的输入
type turnRequest struct {
	plan    *canonicalRequest
	images  []attachment
	timeout time.Duration
	tm      *upstream.Timing
}

// turnResult 一个回合的产出
type turnResult struct {
	content        string
	calls          []toolCall
	thinkingLevel  int
	incomplete     bool       // 上游/连接中断，但已交付了可用内容
	incompleteCode string     // 中断原因码（incomplete = true 时有值）
	incompleteErr  *turnError // 中断的原始错误（用于静音 / 日志；写客户端失败时为 nil）
}

// turnSink 回合产出的去向（由回程交付层实现）
type turnSink interface {
	content(delta string) error    // 正文增量
	call(tc toolCall) error        // 已校验的工具调用
	committed() (bytes, calls int) // 已真正写给客户端的量（未提交时为 0, 0）
	reset()                        // 上游改写了尚未提交的快照：丢弃缓冲
	keepalive()                    // 流式静默期间的保活
}

// streamEvent 响应流上读到的一帧，或流的结束 / 读取失败
type streamEvent struct {
	frame []byte
	err   error
}

var asyncPlaceholderRe = regexp.MustCompile(`(?i)^\s*I'm on it\.\s*Responses with Deep Think can take some time, so check back in a bit\.\s*https?://googleusercontent\.com/agentic_processing_chip/\d+\s*$`)

// runTurn 消费一个已打开的 ProcessQuery 响应流。
func (a *Adapter) runTurn(ctx context.Context, resp *http.Response, req turnRequest, sink turnSink) (*turnResult, error) {
	if req.tm == nil {
		req.tm = upstream.NewTiming()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer resp.Body.Close()

	// 读帧协程：把 gRPC 帧逐个送进通道；结束时投递终止事件（非阻塞，主循环提前
	// 返回时也不会泄漏）
	events := make(chan streamEvent, 8)
	go func() {
		rd := bufio.NewReaderSize(&upstream.FirstByteReader{R: resp.Body, T: req.tm}, 32<<10)
		for {
			frame, err := grpcReadFrame(rd, maxFrameBytes)
			if err != nil {
				select {
				case events <- streamEvent{err: err}:
				default:
				}
				return
			}
			select {
			case events <- streamEvent{frame: frame}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var (
		rawText, snapshotID string
		frames, received    int
		observedMode        = -1 // 响应字段 41：-1 = 尚未给出
		observedThinking    = -1 // 响应字段 42
		downgradeReason     = -1 // 响应字段 31
		attribution         string
		modelComplete       bool
		modelCompleteAt     time.Time
		lastProgress        time.Time // 最近一次正文 / 思考进展时刻
		thoughtHigh         int
		liveSynced          int // rawText 里已喂给解析器的字节数
		liveText            string
		calls               []toolCall
		publishedText       string
		publishedCalls      int
		split               = newFenceSplitter(req.plan.policy)
		ended, eofSeen      bool
		downgrade, refusal  bool
		idleKind            string
	)
	spec := modelSpecs[req.plan.model]
	attributionOK := func() bool { return spec.Attribution == nil || spec.Attribution.MatchString(attribution) }
	modeMatches := func() bool { return observedMode == spec.Mode }
	// verified 上游已证明「就是请求的模型」：档位与归属都对得上，且没有降级 / 拒绝
	verified := func() bool { return !downgrade && !refusal && modeMatches() && attributionOK() }

	// publish 把解析器产出发布给交付层。preview 会让待定正文先行下发，
	// 但可能是围栏开头的片段不暴露，所以发布出去的文本只会单调增长。
	publish := func(items []splitEvent) error {
		for _, it := range items {
			if it.isCall {
				calls = append(calls, it.call)
			} else {
				liveText += it.text
			}
		}
		if !req.plan.parallelCalls && len(calls) > 1 {
			calls = calls[:1] // 客户端要求每回合至多一个调用：只保留首个
		}
		text := liveText + split.preview()
		if !strings.HasPrefix(text, publishedText) {
			cb, cc := sink.committed()
			if snapshotID == "" || cb > 0 || cc > 0 {
				return upstreamFailure("NON_MONOTONIC_NATIVE_RESPONSE", 502, "Gemini 上游改写了已经交付的文本")
			}
			sink.reset()
			publishedText, publishedCalls = "", 0
		}
		if len(text) > len(publishedText) {
			if err := sink.content(text[len(publishedText):]); err != nil {
				return err
			}
			publishedText = text
		}
		for publishedCalls < len(calls) {
			if err := sink.call(calls[publishedCalls]); err != nil {
				return err
			}
			publishedCalls++
		}
		return nil
	}
	feed := func() error {
		if liveSynced >= len(rawText) {
			return nil
		}
		delta := rawText[liveSynced:]
		liveSynced = len(rawText)
		items, err := split.push(delta)
		if err != nil {
			return err
		}
		return publish(items)
	}

	// incompleteAfterCommit 已交付可用内容后的中断结果（调用方已确认 committed
	// 达到门槛）。写客户端失败 / 客户端断开没有上游错误，incompleteErr 为 nil。
	incompleteAfterCommit := func(code string) *turnResult {
		return &turnResult{content: publishedText, calls: calls[:publishedCalls], thinkingLevel: observedThinking,
			incomplete: true, incompleteCode: code}
	}
	// interrupted 回合失败时的收尾：已交付可用内容就平稳结束并标记 incomplete，
	// 否则把错误交还调用方（换候选 / 回传错误码）。
	interrupted := func(code string, cause error) (*turnResult, error) {
		if verified() && !split.closed {
			if items, err := split.flush(); err == nil {
				_ = publish(items) // 未闭合的工具围栏不会放行；写客户端失败也无法再补救
			}
		}
		cb, cc := sink.committed()
		if cb < prefixBytes && cc == 0 {
			return nil, cause
		}
		res := incompleteAfterCommit(code)
		var te *turnError
		if errors.As(cause, &te) {
			res.incompleteErr = te
		}
		return res, nil
	}
	streamFailure := func(code string, status int, msg string) (*turnResult, error) {
		return interrupted(code, upstreamFailure(code, status, msg))
	}

	// idleFor 下一个空闲上限（0 = 不设）、它的起算锚点与类别。
	// 正文到达前没有空闲上限（思考可能很久），只受回合总超时约束。
	idleFor := func() (time.Duration, time.Time, string) {
		switch {
		case modelComplete && verified() && strings.TrimSpace(rawText) != "":
			return a.postCompleteGrace, modelCompleteAt, "post_complete"
		case !verified() || split.state() == splitTool:
			return 0, time.Time{}, ""
		}
		if len(calls) > 0 {
			return a.postToolIdle, lastProgress, "post_tool"
		}
		candidate := liveText + split.preview()
		if strings.TrimSpace(candidate) == "" || softErrorReason(candidate, req.plan) != "" {
			return 0, time.Time{}, ""
		}
		// 声明了工具且未强制：正文停住后模型可能还在思考并随后发起调用，多等一会
		if req.plan.policy.enabled() && req.plan.policy.choice == "auto" {
			return a.postOutputToolIdle, lastProgress, "post_output"
		}
		return a.postOutputIdle, lastProgress, "post_output"
	}
	timer := time.NewTimer(time.Hour)
	keepalive := time.NewTicker(keepaliveInterval)
	defer func() {
		timer.Stop()
		keepalive.Stop()
	}()
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	arm := func() {
		d, anchor, kind := idleFor()
		idleKind = kind
		stopTimer()
		if d <= 0 {
			return
		}
		remaining := d - time.Since(anchor)
		if remaining < 0 {
			remaining = 0
		}
		timer.Reset(remaining)
	}

loop:
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return streamFailure("NATIVE_TURN_TIMEOUT", 504, "Gemini 上游处理超时")
			}
			if cb, cc := sink.committed(); cb > 0 || cc > 0 {
				return incompleteAfterCommit("CLIENT_DISCONNECTED"), nil
			}
			return nil, ctx.Err() // 客户端断开且尚未交付任何内容
		case <-keepalive.C:
			sink.keepalive()
		case <-timer.C:
			if idleKind == "post_complete" {
				// 上游已标记完成却不关流：宽限期到，按正常完成收尾
				ended = true
				break loop
			}
			return streamFailure("NATIVE_TURN_TIMEOUT", 504, "Gemini 上游输出后长时间没有进展（空闲超时）")
		case ev := <-events:
			if ev.err != nil {
				if !errors.Is(ev.err, io.EOF) {
					return streamFailure("NATIVE_STREAM_RESET", 502, "Gemini 上游数据流中断："+ev.err.Error())
				}
				ended, eofSeen = true, true
				break loop
			}
			frames++
			received += len(ev.frame)
			if frames > maxTurnFrames || received > maxTurnWire {
				return streamFailure("NATIVE_RESPONSE_LIMIT", 502, "Gemini 上游响应超过帧数或字节上限")
			}
			tf, ok, derr := decodeFrame(ev.frame)
			if derr != nil {
				var te *turnError
				if errors.As(derr, &te) {
					return interrupted(te.Code, te)
				}
				return nil, derr
			}
			if !ok {
				continue // 续接句柄等非生成事件
			}
			if tf.toolOps {
				return streamFailure("UNSUPPORTED_NATIVE_TOOL_EVENT", 502, "Gemini 上游返回了原生工具操作（本网关不承接）")
			}
			// 模型身份证据：字段 41 是档位，字段 30 是归属文本
			hasContent := (tf.hasText && tf.text != "") || tf.completed
			if tf.resolvedMode >= 0 && (tf.resolvedMode != 0 || hasContent || observedMode == -1) {
				observedMode = tf.resolvedMode
			}
			if (tf.resolvedMode > 0 || hasContent) && tf.attribution != "" {
				attribution = tf.attribution
				if !attributionOK() {
					downgrade = true
				}
			}
			if observedMode > 0 && observedMode != spec.Mode {
				downgrade = true
			}
			if tf.fallback == 1 || tf.downgradeReason > 0 {
				downgrade = true
			}
			if tf.thinkingLevel >= 0 {
				if tf.thinkingLevel > 4 {
					return streamFailure("INVALID_NATIVE_THINKING_LEVEL", 502, "Gemini 上游返回了无法识别的思考档位")
				}
				observedThinking = tf.thinkingLevel
			}
			modelComplete = modelComplete || tf.completed
			if tf.downgradeReason >= 0 {
				downgradeReason = tf.downgradeReason
			}
			if downgrade {
				// 一旦证明模型不对就立刻停：不为一个永远不能当成功返回的降级答案付费
				if code := quotaDowngradeCode(downgradeReason); code != "" {
					return streamFailure(code, 429, "Gemini 套餐额度不足，上游把模型降级了")
				}
				return streamFailure("MODEL_DOWNGRADE_REJECTED", 502, "Gemini 上游返回的模型与请求的不一致（已拒绝降级响应）")
			}
			if tf.refusal {
				refusal = true
				return streamFailure("NATIVE_RESPONSE_REFUSED", 422, "Gemini 拒绝了本次请求")
			}
			// 正文是整文快照：要么延续上一版，要么是同一响应 ID 的改写
			if tf.hasText {
				id := tf.responseID
				if snapshotID != "" && id != snapshotID {
					return streamFailure("INVALID_NATIVE_RESPONSE", 502, "Gemini 上游的文本快照缺少一致的响应标识")
				}
				if !strings.HasPrefix(tf.text, rawText) {
					cb, cc := sink.committed()
					if id == "" || snapshotID != id || cb > 0 || cc > 0 {
						return streamFailure("NON_MONOTONIC_NATIVE_RESPONSE", 502, "Gemini 上游改写了已经交付的文本")
					}
					sink.reset()
					split = newFenceSplitter(req.plan.policy)
					liveText, calls, publishedText, publishedCalls, liveSynced = "", nil, "", 0, 0
				}
				if id != "" {
					snapshotID = id
				}
				if tf.text != rawText {
					modelComplete, modelCompleteAt = tf.completed, time.Now()
					lastProgress = time.Now()
				}
				rawText = tf.text
			} else if snapshotID != "" && tf.responseID != "" && tf.responseID != snapshotID {
				return streamFailure("INVALID_NATIVE_RESPONSE", 502, "Gemini 上游的响应标识发生了变化")
			}
			if tf.thoughtBytes > thoughtHigh {
				thoughtHigh = tf.thoughtBytes
				if !lastProgress.IsZero() {
					lastProgress = time.Now() // 思考仍在推进也算进展
				}
			}
			if verified() {
				if err := feed(); err != nil {
					var te *turnError
					if !errors.As(err, &te) {
						if cb, cc := sink.committed(); cb > 0 || cc > 0 {
							return incompleteAfterCommit("CLIENT_WRITE_FAILED"), nil
						}
						return nil, err // 首段就写客户端失败：交还候选循环
					}
					return interrupted(te.Code, te)
				}
				if split.suppressed && len(calls) > 0 {
					// 模型在有效调用之后开始臆造 [Caller tool result]：立刻停掉上游
					ended, modelComplete = true, true
					break loop
				}
			}
			arm()
		}
	}
	if eofSeen { // 流正常结束：再看一次尾部状态（出错时 gRPC 状态在尾部帧里）
		if f := statusFailure(readRPCStatus(resp)); f != nil {
			return interrupted(f.Code, f)
		}
	}

	// ── 校验阶段：一次 RPC 关流不等于模型完成了生成 ──
	switch {
	case downgrade, refusal:
		return streamFailure("MODEL_DOWNGRADE_REJECTED", 502, "Gemini 上游返回的模型与请求的不一致")
	case !ended, !modelComplete:
		return streamFailure("INCOMPLETE_NATIVE_RESPONSE", 502, "Gemini 上游在生成完成前结束了响应")
	case asyncPlaceholderRe.MatchString(rawText):
		return streamFailure("NATIVE_ASYNC_RESPONSE_UNSUPPORTED", 422, "Gemini 返回了异步 Deep Think 的占位回复（本网关不支持）")
	case !modeMatches():
		return streamFailure("MISSING_NATIVE_MODE_EVIDENCE", 502, "Gemini 上游没有给出可验证的模型档位")
	case !attributionOK():
		return streamFailure("MODEL_DOWNGRADE_REJECTED", 502, "Gemini 上游返回的模型与请求的不一致")
	case strings.TrimSpace(rawText) == "":
		return streamFailure("EMPTY_NATIVE_RESPONSE", 502, "Gemini 上游返回了空响应")
	}
	if liveSynced < len(rawText) {
		delta := rawText[liveSynced:]
		liveSynced = len(rawText)
		items, err := split.push(delta)
		if err != nil {
			return interrupted(err.(*turnError).Code, err) //nolint:errorlint // push 只返回 *turnError
		}
		if err := publish(items); err != nil {
			if cb, cc := sink.committed(); cb > 0 || cc > 0 {
				return incompleteAfterCommit("CLIENT_WRITE_FAILED"), nil
			}
			return nil, err
		}
	}
	items, ferr := split.flush()
	if ferr != nil {
		var te *turnError
		if errors.As(ferr, &te) {
			return interrupted(te.Code, te)
		}
		return nil, ferr
	}
	if err := publish(items); err != nil {
		if cb, cc := sink.committed(); cb > 0 || cc > 0 {
			return incompleteAfterCommit("CLIENT_WRITE_FAILED"), nil
		}
		return nil, err
	}
	content := liveText
	if len(calls) == 0 {
		if reason := softErrorReason(content, req.plan); reason != "" {
			return streamFailure("NATIVE_SOFT_ERROR_RESPONSE", 502, "Gemini 返回了通用的出错话术，没有完成请求")
		}
	}
	if len(calls) == 0 && strings.TrimSpace(content) == "" {
		return streamFailure("EMPTY_NATIVE_RESPONSE", 502, "Gemini 上游返回了空响应")
	}
	if rf := req.plan.responseFormat; rf != nil && len(calls) == 0 {
		// 结构化输出终检：不合法就不交付；去掉围栏后与原文不同则整体重发
		validated, verr := validateStructuredOutput(content, rf)
		if verr != nil {
			return interrupted(verr.(*turnError).Code, verr) //nolint:errorlint // validateStructuredOutput 只返回 *turnError
		}
		if validated != content {
			sink.reset()
			if err := sink.content(validated); err != nil {
				return nil, err
			}
			content = validated
		}
	}
	return &turnResult{content: content, calls: calls, thinkingLevel: observedThinking}, nil
}

// openTurn 发起 ProcessQuery 请求；状态码非 2xx 时读出错误体并归类
func (a *Adapter) openTurn(ctx context.Context, cli *http.Client, token string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.robinOrigin()+processQueryPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setRPCHeaders(req.Header, token, timeout)
	resp, err := cli.Do(req)
	if err != nil {
		return nil, &turnError{Code: "NATIVE_CONNECTION_FAILED", Status: 502, Message: "连接 Gemini 上游失败：" + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, statusFailure(readRPCStatus(resp))
	}
	return resp, nil
}

// softErrorReason 整段回复恰好是上游的通用出错话术时返回 generic_error_offer。
// 只比对整段回复，不扫描工具实参里的错误词，也不把引用的数据判成失败。
var (
	genericErrorOffers = []string{
		"I seem to be encountering an error. Can I try something else for you?",
		"I'm having a hard time fulfilling your request. Can I help you with something else instead?",
		"I encountered an error doing what you asked. Could you try again?",
	}
	whitespaceRe = regexp.MustCompile(`\s+`)
)

func softErrorReason(text string, req *canonicalRequest) string {
	if len([]rune(text)) > 1024 {
		return ""
	}
	signature := strings.TrimSpace(whitespaceRe.ReplaceAllString(text, " "))
	hit := ""
	for _, offer := range genericErrorOffers {
		if signature == offer {
			hit = offer
			break
		}
	}
	if hit == "" {
		return ""
	}
	fold := func(s string) bool {
		return strings.Contains(strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " ")), hit)
	}
	var latestUser string
	for _, m := range req.messages {
		switch m.role {
		case "user":
			latestUser = m.content
		case "system", "developer":
			if fold(m.content) {
				return ""
			}
		}
	}
	if fold(latestUser) {
		return ""
	}
	// 字面数据豁免：最近一条工具结果里引用了这句话
	if n := len(req.messages); n > 0 && req.messages[n-1].role == "tool" && fold(req.messages[n-1].content) {
		return ""
	}
	return "generic_error_offer"
}
