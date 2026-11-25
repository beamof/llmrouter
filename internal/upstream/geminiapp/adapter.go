package geminiapp

// 候选处理入口（由网关的候选循环调用）：请求归一化 → 懒续期 → 图片上传 →
// 发起回合（凭据被拒时强制续期重试一次）→ 回程按客户端协议交付。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/wire"
)

// Attempt 实现 upstream.Adapter。
func (a *Adapter) Attempt(att *upstream.Attempt) error {
	return a.attempt(att.Writer, att.Request, att.Candidate, att.Body, att.KeyID,
		att.Protocol == domain.ProtocolResponses, att.Timing, att.Usage)
}

// attempt 处理一个 Gemini App 候选的完整生命周期。
func (a *Adapter) attempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *domain.CallSample) error {
	defer a.rt.Store.Release(c.Model.ID)
	cred := Cred(c.Provider)
	if cred == nil || strings.TrimSpace(cred.RefreshToken) == "" {
		err := errors.New("未登录 Google 账号（provider 缺少 Gemini App 凭证）")
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name, Message: err.Error()})
		return err
	}
	plan, err := buildPlan(sendBody, responsesMode)
	if err != nil {
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindBadRequest, Status: errorStatus(err), KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name, Message: "Gemini App 请求归一化失败", Detail: err.Error()})
		return a.statusError(err)
	}
	if sm != nil {
		sm.EffortSent = "thinking=" + itoa(plan.thinkingLevel) // 该上游没有 effort 字段，实发的是思考档位
	}
	wantStream := wire.BodyStream(sendBody)
	d := newDelivery(w, plan, responsesMode, wantStream, responsesMode || bodyIncludeUsage(sendBody))
	cli, err := a.rt.Clients.For(c.Provider, 0)
	if err != nil {
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "上游代理配置非法", Detail: err.Error()})
		return err
	}
	// 懒续期：临期自动换新，失败不阻断（沿用现有 token，上游拒了再强制续期）
	if cred, err = a.usableToken(c.Provider); err != nil {
		return a.failure(c, keyID, err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.turnTimeout)
	defer cancel()
	attachments, err := a.uploadImages(ctx, cli, cred.AccessToken, plan.images)
	if err != nil {
		return a.failure(c, keyID, err)
	}
	body := buildTurnWire(plan.prompt, attachments, modelSpecs[plan.model].Mode, plan.thinkingLevel)
	req := turnRequest{plan: plan, images: attachments, timeout: a.turnTimeout, tm: tm}

	// 发请求并分类。凭据级拒绝（401）强制续期后原候选重试一次
	var resp *http.Response
	for refreshed := false; ; {
		resp, err = a.openTurn(ctx, cli, cred.AccessToken, body, a.turnTimeout)
		if err == nil {
			break
		}
		var te *turnError
		if refreshed || !errors.As(err, &te) || te.Code != "NATIVE_AUTH_REJECTED" {
			return a.failure(c, keyID, err)
		}
		nc, rerr := a.refreshToken(c.Provider, true)
		if rerr != nil {
			a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, Status: http.StatusUnauthorized, KeyID: keyID,
				ProviderID: c.Provider.ID, Model: c.Model.Name,
				Message: "Gemini 凭据被拒且续期失败，请重新登录 Google 账号", Detail: rerr.Error()})
			return a.statusError(&turnError{Code: "NATIVE_AUTH_REJECTED", Status: http.StatusUnauthorized,
				Message: "Gemini 账号授权已失效，请重新登录 Google 账号", detail: rerr.Error()})
		}
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, Status: http.StatusUnauthorized, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "Gemini 凭据被拒，已自动续期并重试该候选"})
		cred = nc
		refreshed = true
	}
	res, rerr := a.runTurn(ctx, resp, req, d)
	if rerr != nil {
		return a.failure(c, keyID, rerr)
	}
	a.reportIncomplete(c, keyID, res)
	usage, werr := d.finish(res)
	if sm != nil {
		sm.Apply(tm.Sample(usage, wantStream))
	}
	if werr != nil {
		// 响应头与部分内容已发出，无法换候选；记一条日志便于排查截断
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "Gemini 流式响应写出失败（客户端可能已断开）", Detail: werr.Error()})
	}
	return nil
}

// buildPlan 把客户端请求（chat 或 responses 形态）归一化
func buildPlan(sendBody []byte, responsesMode bool) (*canonicalRequest, error) {
	var body map[string]any
	if err := json.Unmarshal(sendBody, &body); err != nil || body == nil {
		return nil, badRequest("INVALID_REQUEST", "请求体不是合法 JSON 对象")
	}
	if !responsesMode {
		return normalizeChat(body, nil, false)
	}
	chat, identities, err := responsesToChat(body)
	if err != nil {
		return nil, err
	}
	return normalizeChat(chat, identities, true)
}

// bodyIncludeUsage chat 客户端是否显式要求 usage 帧（stream_options.include_usage）
func bodyIncludeUsage(body []byte) bool {
	var t struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	json.Unmarshal(body, &t)
	return t.StreamOptions.IncludeUsage
}

// failure 记录一次上游失败并把错误折成可透传的状态错误。私有适配器自行记日志
// （网关的 logAttemptFailure 只归因标准上游）。
func (a *Adapter) failure(c Candidate, keyID string, err error) error {
	te := &turnError{}
	if !errors.As(err, &te) {
		if errors.Is(err, context.Canceled) {
			return err // 客户端已断开：不当作上游异常
		}
		a.rt.Logf(LogEntry{Level: "error", Kind: domain.KindUpstream, KeyID: keyID,
			ProviderID: c.Provider.ID, Model: c.Model.Name,
			Message: "Gemini App 调用失败", Detail: err.Error()})
		if errors.Is(err, errAuthRevoked) {
			return a.statusError(&turnError{Code: "NATIVE_AUTH_REVOKED", Status: http.StatusUnauthorized,
				Message: "Gemini 账号授权已失效，请重新登录 Google 账号", detail: err.Error()})
		}
		return err
	}
	a.logFailure(c, keyID, te)
	return a.statusError(te)
}

// logFailure 记录一次上游失败；429 / 套餐额度不足时自动静音该模型条目
// （瞬时限流 1 分钟，套餐额度按 5 小时窗口）。
func (a *Adapter) logFailure(c Candidate, keyID string, te *turnError) {
	entry := LogEntry{Level: "error", Kind: domain.KindUpstream, Status: te.Status, KeyID: keyID,
		ProviderID: c.Provider.ID, Model: c.Model.Name, Message: te.Message, Detail: te.detail}
	if te.Status == http.StatusTooManyRequests {
		mute := time.Minute
		if te.Code == "NATIVE_QUOTA_EXHAUSTED" || te.Code == "NATIVE_QUOTA_INSUFFICIENT" {
			mute = domain.RateLimitMute
		}
		until, _ := a.rt.Store.MuteOn429(c.Model.ID, mute, te.Message)
		entry.Level, entry.Kind = "warn", domain.KindMuted
		entry.Message = fmt.Sprintf("%s，已自动静音至 %s", te.Message, until.Local().Format("01-02 15:04"))
	}
	a.rt.Logf(entry)
}

// reportIncomplete 记录「已交付部分内容后中断」：429 / 套餐额度不足仍然静音，
// 其余中断只记 warn（客户端已经拿到 incomplete 标记，无法再换候选）。
func (a *Adapter) reportIncomplete(c Candidate, keyID string, res *turnResult) {
	if res == nil || !res.incomplete {
		return
	}
	if te := res.incompleteErr; te != nil && te.Status == http.StatusTooManyRequests {
		a.logFailure(c, keyID, te)
		return
	}
	entry := LogEntry{Level: "warn", Kind: domain.KindUpstream, KeyID: keyID,
		ProviderID: c.Provider.ID, Model: c.Model.Name,
		Message: "Gemini 响应中断（已交付部分内容）", Detail: res.incompleteCode}
	if te := res.incompleteErr; te != nil {
		entry.Status = te.Status
		if te.detail != "" {
			entry.Detail = te.Code + ": " + te.detail
		}
	}
	a.rt.Logf(entry)
}

// statusError 把回合错误折成直连可透传的 StatusError（状态码 + JSON 错误体）
func (a *Adapter) statusError(err error) error {
	te := &turnError{}
	if !errors.As(err, &te) {
		return err
	}
	body, _ := encodeCompactJSON(map[string]any{"error": map[string]string{
		"message": te.Message, "type": errorType(te.Status), "code": te.Code,
	}})
	return &upstream.StatusError{Status: te.Status, Body: body}
}

func errorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status < 500:
		return "invalid_request_error"
	}
	return "api_error"
}

func errorStatus(err error) int {
	te := &turnError{}
	if errors.As(err, &te) && te.Status >= 400 && te.Status <= 599 {
		return te.Status
	}
	return http.StatusBadGateway
}

// Probe 连通性探测：确保凭证新鲜后只读查询账号用量（GetUsageInfo），
// 不发对话、不消耗额度；顺带核对模型名。
func (a *Adapter) Probe(ctx context.Context, p domain.Provider, model string) (upstream.ProbeResult, error) {
	start := a.rt.NowTime()
	if cred := Cred(p); cred == nil || strings.TrimSpace(cred.RefreshToken) == "" {
		return upstream.ProbeResult{Level: "fail", Message: "未登录 Google 账号"}, nil
	}
	res := upstream.ProbeResult{Level: "ok", Message: "账号可用"}
	if _, ok := canonicalModel(model); !ok {
		res.Level = "warn"
		res.Message = fmt.Sprintf("账号可用，但 %q 不是 Gemini App 支持的模型（可用：%s）", model, strings.Join(nativeModels, " / "))
	}
	cred, err := a.usableToken(p)
	if err != nil {
		return upstream.ProbeResult{Level: "fail", Message: "凭证不可用: " + err.Error()}, nil
	}
	cli, err := a.rt.Clients.For(p, 15*time.Second)
	if err != nil {
		return upstream.ProbeResult{Level: "fail", Message: err.Error()}, nil
	}
	usage, err := a.fetchUsage(ctx, cli, cred.AccessToken)
	a.saveUsage(p.ID, usage, err)
	res.LatencyMS = a.rt.NowTime().Sub(start).Milliseconds()
	if err != nil {
		res.Level = "warn"
		res.Message = "账号可用，但用量查询失败: " + err.Error()
		return res, nil
	}
	if usage.Tier != "" {
		res.Message += "（套餐 " + tierLabel(usage.Tier) + "）"
	}
	return res, nil
}

// tierLabel 订阅档位的展示名
func tierLabel(tier string) string {
	switch tier {
	case "free":
		return "Free"
	case "pro":
		return "AI Pro"
	case "ultra":
		return "AI Ultra"
	case "plus":
		return "AI Plus"
	case "neon":
		return "Ultra / Neon"
	}
	return "未知"
}

// ListModels 内置模型目录（后台「添加模型」下拉用；上游没有目录端点）
func (a *Adapter) ListModels(context.Context, domain.Provider) ([]string, error) {
	return append([]string(nil), nativeModels...), nil
}
