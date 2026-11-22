package codex

// ─────────────────── Codex 数据面：出站装配、SSE 回程翻译与候选处理 ───────────────────
//
// 上游是公共 Responses API（api.openai.com/v1/responses），Bearer 用 OAuth access
// token。请求恒为流式（SIWC 要求 stream:true / store:false），回程是标准
// OpenAI Responses SSE（data: {type:…}）。客户端协议四种组合的输出矩阵：
//
//	客户端\出站 | chat 流式           | chat 非流式         | responses 流式     | responses 非流式
//	处理方式    | 事件→chat 分片      | 分片→聚合           | 事件重新帧化透传   | 收集 response.completed
//
// 凭据被拒（401）先强制续期再原候选重试一次；套餐用量达上限（429 +
// subscription_sharing_usage_limit_exceeded）按网关 429 语义自动静音该模型条目。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/wire"
)

// codexAttempt Codex 候选的处理分支：装配出站体 → 懒续期 → 发请求（凭据被拒时
// 强制续期重试一次，用量超限时静音该模型）→ 回程按客户端协议翻译。
// 名额释放与失败日志与 zedAttempt 同构。
func (a *Adapter) attempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *domain.CallSample) error {
	defer a.rt.Store.Release(c.Model.ID)
	cred := codexOf(c.Provider)
	if cred == nil || cred.AccessToken == "" {
		err := fmt.Errorf("未登录 ChatGPT 账号（provider 缺少 Codex 凭证）")
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name, Message: err.Error()})
		return err
	}
	// 懒续期：临期自动换新，失败不阻断（沿用现有 token，上游拒了再强制续期）
	if codexTokenExpiring(cred) {
		if nc, err := a.refreshToken(c.Provider, false); err == nil {
			cred = nc
		} else {
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Codex 凭证临期续期失败（沿用现有 token）", Detail: err.Error()})
		}
	}
	model := c.Model.Name
	outBody, plan, err := codexBuildOutbound(sendBody, responsesMode, model)
	if err != nil {
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindBadRequest, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "构造 Codex 出站请求失败", Detail: err.Error()})
		return err
	}
	if sm != nil {
		sm.EffortSent = plan.effort
	}
	wantStream := wire.BodyStream(sendBody)
	includeUsage := !wantStream || codexBodyIncludeUsage(sendBody)
	cli, err := a.rt.Clients.For(c.Provider, 0)
	if err != nil {
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "上游代理配置非法", Detail: err.Error()})
		return err
	}
	url := strings.TrimRight(c.Provider.BaseURL, "/")
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(outBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cred.AccessToken))
		return cli.Do(req)
	}
	// 发请求并分类。凭据级拒绝（401）强制续期后原候选重试一次；
	// 套餐用量达上限（429 + usage_limit_exceeded）静音该模型条目，其余 4xx/5xx 原样透传。
	var resp *http.Response
	refreshed := false
	for {
		resp, err = do()
		if err != nil {
			a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "连接上游失败（网络错误或超时）", Detail: err.Error()})
			return err
		}
		if resp.StatusCode < 400 {
			break
		}
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && !refreshed {
			nc, rerr := a.refreshToken(c.Provider, true)
			if rerr != nil {
				a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, Status: resp.StatusCode, KeyID: keyID,
					ProviderID: c.Provider.ID, Model: c.Model.Name,
					Message: "Codex 凭据被拒且续期失败，请重新登录", Detail: rerr.Error()})
				return &upstream.StatusError{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: eb}
			}
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Codex 凭据被拒，已自动续期并重试该候选"})
			cred = nc
			refreshed = true
			continue
		}
		if codexUsageLimitExceeded(resp.StatusCode, eb) {
			until, _ := a.rt.Store.MuteOn429(c.Model.ID, domain.RateLimitMute, "ChatGPT 套餐用量已达上限（429）")
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindMuted, Status: resp.StatusCode, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: fmt.Sprintf("ChatGPT 套餐用量已达上限，已静音至 %s（可在 ChatGPT 设置里查看用量）", until.Local().Format("01-02 15:04")),
				Detail:  upstream.ErrSnippet(eb)})
			return &upstream.StatusError{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: eb}
		}
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, Status: resp.StatusCode, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: fmt.Sprintf("Codex 上游返回 HTTP %d", resp.StatusCode), Detail: upstream.ErrSnippet(eb)})
		return &upstream.StatusError{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: eb}
	}
	defer resp.Body.Close()
	// 首字节在翻译之前打点：上游发出的第一个 SSE 分片即算响应到达
	upBody := &upstream.FirstByteReader{R: resp.Body, T: tm}
	usage, wrote, rerr := a.relay(upBody, w, model, wantStream, responsesMode, includeUsage)
	if sm != nil {
		sm.Apply(tm.Sample(usage, wantStream))
	}
	if rerr != nil {
		if wrote {
			// 响应头与部分内容已发出，无法换候选；记一条日志便于排查截断
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Codex 流式响应中断（已渲染部分内容）", Detail: rerr.Error()})
			return nil
		}
		return rerr
	}
	return nil
}

// codexWirePlan 一次出站请求的形态
type codexWirePlan struct {
	effort string // 实际下发的思考档位；"" = 未下发
}

// codexBuildOutbound 把客户端请求（chat 或 responses 形态）装配成 SIWC 合规的
// Responses 出站体。responses 客户端归一化原始 body（保留 Codex 语义，避免两次
// 有损转换），chat 客户端先转 responses 再归一化。
func codexBuildOutbound(sendBody []byte, responsesMode bool, model string) ([]byte, *codexWirePlan, error) {
	plan := &codexWirePlan{}
	var body map[string]any
	if responsesMode {
		if err := json.Unmarshal(sendBody, &body); err != nil || body == nil {
			return nil, nil, fmt.Errorf("/responses 请求体不是合法 JSON 对象")
		}
	} else {
		var chatBody map[string]any
		if err := json.Unmarshal(sendBody, &chatBody); err != nil || chatBody == nil {
			return nil, nil, fmt.Errorf("请求体不是合法 JSON 对象")
		}
		r, err := codexChatToResponses(chatBody, model)
		if err != nil {
			return nil, nil, err
		}
		body = r
	}
	body["model"] = model
	codexNormalizeResponses(body)
	// 实发档位取自归一化后的出站体：responses 客户端原样保留 reasoning.effort，
	// chat 客户端由 reasoning_effort 映射而来
	if r, ok := body["reasoning"].(map[string]any); ok {
		if s, _ := r["effort"].(string); strings.TrimSpace(s) != "" {
			plan.effort = strings.TrimSpace(s)
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return b, plan, nil
}

// codexBodyIncludeUsage chat 客户端是否显式要求 usage 帧（stream_options.include_usage）
func codexBodyIncludeUsage(body []byte) bool {
	var t struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	json.Unmarshal(body, &t)
	return t.StreamOptions.IncludeUsage
}

// codexRelay 把上游 Responses SSE 流翻译给客户端。返回（记账用量, 是否已向客户端
// 写字节, 错误）。已写字节后出错只能收尾（调用方记 warn 不换候选）；未写字节时出错
// 交还调用方换候选。上游事件以 data: 行承载 JSON（type 字段在 payload 里）。
func (a *Adapter) relay(r io.Reader, w http.ResponseWriter, model string, wantStream, responsesMode, includeUsage bool) (Usage, bool, error) {
	sc := bufio.NewReader(r)
	fw := wire.FlushWriter{W: w}
	var (
		agg       *wire.Aggregator        // chat 非流式聚合器
		fromResp  *codexChatFromResponses // 事件 → chat 分片
		finalResp map[string]any          // responses 非流式：response.completed 的对象
		streamUse Usage                   // responses 流式：从 completed 事件嗅探
		failErr   error                   // responses 非流式：response.failed 的错误
		started   bool
		done      bool // 已见终事件（completed / incomplete / failed）
	)
	switch {
	case responsesMode && wantStream:
		// responses 流式：事件重新帧化透传
	case responsesMode:
		// responses 非流式：收集最终 response 对象
	case wantStream:
		fromResp = newCodexChatFromResponses(fw, model, includeUsage)
	default:
		agg = &wire.Aggregator{}
		fromResp = newCodexChatFromResponses(agg, model, true)
	}
	// start 首个事件前才写响应头：之前的错误阶段仍可换候选
	start := func() {
		if started || !wantStream {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}
	// finish 正常收尾：写终片/聚合结果，返回记账用量
	finish := func() (Usage, error) {
		switch {
		case fromResp != nil && agg != nil: // chat 非流式
			if _, terr := fromResp.finish(); terr != nil {
				return Usage{}, terr
			}
			out, u := agg.Result(model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
			return u, nil
		case fromResp != nil: // chat 流式
			return fromResp.finish()
		case wantStream: // responses 流式（终事件已透传）
			return streamUse, nil
		default: // responses 非流式
			if finalResp == nil {
				if failErr != nil {
					return Usage{}, failErr
				}
				return Usage{}, errors.New("Codex 流结束但缺少 response.completed 事件")
			}
			b, err := json.Marshal(finalResp)
			if err != nil {
				return Usage{}, err
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(b)
			return codexUsageOfResponses(finalResp["usage"]), nil
		}
	}
	for {
		line, readErr := sc.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] != ':' { // SSE 注释行跳过
			if after, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				payload := bytes.TrimSpace(after)
				if bytes.Equal(payload, []byte("[DONE]")) {
					continue // Responses 流不发 [DONE]，出现也只当噪声
				}
				var ev map[string]any
				if json.Unmarshal(payload, &ev) != nil || ev == nil {
					continue // 无法解析的帧跳过（半行/心跳）
				}
				evType := codexStr(ev["type"])
				start()
				switch {
				case fromResp != nil:
					if err := fromResp.event(ev); err != nil {
						return Usage{}, started, err
					}
				case wantStream:
					// 事件重新帧化透传（payload 原样转发）
					started = true
					if _, err := fmt.Fprintf(fw, "event: %s\ndata: %s\n\n", evType, payload); err != nil {
						return Usage{}, started, err
					}
				}
				switch evType {
				case "response.completed", "response.incomplete":
					resp := codexMap(ev["response"])
					if !wantStream && responsesMode {
						finalResp = resp
					} else {
						streamUse = codexUsageOfResponses(resp["usage"])
					}
				case "response.failed":
					if !wantStream && responsesMode {
						resp := codexMap(ev["response"])
						msg := "上游返回错误"
						if e := codexMap(resp["error"]); e != nil {
							if m := codexStr(e["message"]); m != "" {
								msg = m
							}
							if c := codexStr(e["code"]); c != "" {
								msg = c + ": " + msg
							}
						}
						failErr = fmt.Errorf("%s", msg)
					}
				}
				if evType == "response.completed" || evType == "response.incomplete" || evType == "response.failed" {
					done = true
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				u, ferr := finish()
				return u, started, ferr
			}
			if started {
				u, _ := finish()
				return u, true, fmt.Errorf("读取 Codex 上游流失败: %v", readErr)
			}
			return Usage{}, false, fmt.Errorf("读取 Codex 上游流失败: %v", readErr)
		}
		if done {
			// 终事件后上游会关流；提前收尾避免等 EOF（部分实现会挂着连接）
			u, ferr := finish()
			return u, started, ferr
		}
	}
}
