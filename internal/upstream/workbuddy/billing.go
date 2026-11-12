package workbuddy

// ─────────────── WorkBuddy 计费：签到与余额查询 ───────────────
//
// 协议对齐 ccLoad internal/codebuddyauth（其 UPSTREAM.md 有 2026-09-12 三个真实
// 渠道的实测记录）：
//   · 签到：仅国内版个人账号，POST {endpoint}/v2/billing/meter/daily-checkin（空
//     JSON）。上游把所有非成功结果都编成 HTTP 400 + code 10001，只有 msg 区分语义：
//     「今天已签到，请明天再来 / 今日已签到」= 幂等成功；国际版「签到活动未开启或
//     已过期」、企业版「企业账号不支持该操作」都表示签到并未发生，按失败处理。
//     幂等判定依赖上游中文文案，上游改措辞会退化为报错而非假成功。
//   · 个人余额：POST {endpoint}/v2/billing/meter/get-user-resource（当前有效套餐
//     筛选条件），按周期剩余量聚合，周期字段缺失回退总剩余量，负值截零；缺总额
//     时不输出 total/used（前端不画进度）。
//   · 企业余额（enterpriseId 非空）：POST {endpoint}/v2/billing/meter/get-enterprise-
//     user-usage（空 JSON）。credit=周期已用积分、limitNum=成员周期总额度，剩余 =
//     max(0, limitNum-credit)，limitNum=-1 表示无限额度；积分保留小数，个人资源包
//     的空 Accounts 不能当零余额。
// 计费请求沿用控制面（续期/目录）的官方客户端来源指纹：上游对无 UA/Origin 的裸请求
// 风控更严，计费前先补齐。refresh token 只允许出现在续期接口，签到/余额请求绝不携带。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"llmrouter/internal/domain"
	"math"
	rand "math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// WBUsage 一次余额查询的安全快照（存进凭证，随状态文件落盘）

// checkinBusinessCode 签到接口的通用业务拒绝码（所有非成功都是它）
const checkinBusinessCode = 10001

// checkinAlreadyMarker 上游「当天已签到」幂等成功的文案特征（上游只有这一种措辞族）
const checkinAlreadyMarker = "已签到"

// wbAPIError 计费上游的业务错误（保留状态码与业务码供语义判定）
type wbAPIError struct {
	Status int
	Code   int64
	Msg    string
}

func (e *wbAPIError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("上游 HTTP %d（code %d）: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("上游 HTTP %d（code %d）", e.Status, e.Code)
}

// wbBillingHeader 计费接口的请求头：官方客户端来源指纹 + 鉴权/账号身份。
// 与续期/目录共用控制面头族；refresh token 只出现在续期接口，这里不带。
func wbBillingHeader(e *wbEdition, cred *WorkBuddyCred) http.Header {
	return wbFrameworkHeader(e, cred)
}

// billingPost 发一个计费 POST 并解开 {code, msg, data} 信封；code 非 0 时返回
// *wbAPIError。发之前按需懒续期（与对话路径一致）。
func (a *Adapter) billingPost(p Provider, path string, body []byte) (map[string]any, error) {
	e, cred := wbOf(p)
	if e == nil || cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("未导入 WorkBuddy 凭证")
	}
	if cred.RefreshToken != "" && wbTokenExpiring(cred.Expires()) {
		if nc, rerr := a.refreshToken(p, false); rerr == nil {
			cred = nc
		} else {
			// 不能静默：临期续期失败后仍会拿旧 token 发计费请求，上游多半回 401/5xx。
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindUpstream, ProviderID: p.ID,
				Message: "WorkBuddy token 临期续期失败（沿用现有 token）", Detail: rerr.Error()})
		}
	}
	req, err := http.NewRequest(http.MethodPost, wbEndpoint(p)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = wbBillingHeader(e, cred)
	cli, cerr := a.rt.Clients.For(p, 20*time.Second)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("计费请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		Code    *float64        `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		// 非 JSON 响应（WAF/代理 HTML、空体）带上开头片段，便于区分风控页与上游故障。
		if resp.StatusCode >= 400 {
			return nil, &wbAPIError{Status: resp.StatusCode, Code: -1,
				Msg: "非 JSON 响应: " + wbBodySnippet(b)}
		}
		return nil, fmt.Errorf("计费响应不是合法 JSON（HTTP %d）: %s", resp.StatusCode, wbBodySnippet(b))
	}
	// HTTP 层已报错但信封里没有可用的业务码（code 缺失或为 0）：不能当成功吞掉。
	if resp.StatusCode >= 400 && (v.Code == nil || *v.Code == 0) {
		msg := v.Msg
		if msg == "" {
			msg = v.Message
		}
		if msg == "" {
			msg = wbBodySnippet(b)
		}
		code := int64(-1)
		if v.Code != nil {
			code = int64(*v.Code)
		}
		return nil, &wbAPIError{Status: resp.StatusCode, Code: code, Msg: msg}
	}
	if v.Code != nil && *v.Code != 0 {
		msg := v.Msg
		if msg == "" {
			msg = v.Message
		}
		if msg == "" && resp.StatusCode >= 500 {
			msg = wbBodySnippet(b)
		}
		return nil, &wbAPIError{Status: resp.StatusCode, Code: int64(*v.Code), Msg: msg}
	}
	var data map[string]any
	if len(v.Data) > 0 && json.Unmarshal(v.Data, &data) == nil {
		return data, nil
	}
	return map[string]any{}, nil
}

// wbBodySnippet 截取响应体开头，最多 300 个字符（按 rune 截断，不会切坏中文）。
func wbBodySnippet(b []byte) string {
	return fmt.Sprintf("%.300s", strings.TrimSpace(string(b)))
}

// checkin 国内版个人账号每日签到。already=true 表示当天已签过（幂等成功）。
// 企业账号不走个人签到（上游拒绝），由调用方分流。
// 成功与「上游说已签过」都会把当天记为已签到，当天后续时点不再请求签到接口。
// 凭证失效（401 / 12153）时强制续期一次并重试，避免过期 token 让签到直接失败。
func (a *Adapter) checkin(p Provider) (already bool, err error) {
	already, err = a.checkinOnce(p)
	if err == nil {
		return already, nil
	}
	var ae *wbAPIError
	if !errors.As(err, &ae) || !wbCheckinAuthError(ae) {
		return already, err
	}
	if _, rerr := a.refreshToken(p, true); rerr != nil {
		a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindCheckin, ProviderID: p.ID,
			Message: "WorkBuddy 签到凭证失效，续期失败", Detail: rerr.Error()})
		return already, err
	}
	a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindCheckin, ProviderID: p.ID,
		Message: "WorkBuddy 签到凭证已失效，已自动续期并重试"})
	return a.checkinOnce(p)
}

// checkinOnce 发一次签到请求并按上游文案判定幂等。
func (a *Adapter) checkinOnce(p Provider) (already bool, err error) {
	_, err = a.billingPost(p, "/v2/billing/meter/daily-checkin", []byte("{}"))
	if err == nil {
		a.rt.Store.UpdateWBCheckinDay(p.ID, checkinDay(time.Now()))
		return false, nil
	}
	var ae *wbAPIError
	if errors.As(err, &ae) && ae.Code == checkinBusinessCode && strings.Contains(ae.Msg, checkinAlreadyMarker) {
		// 上游说今天已签过（可能来自官方客户端）：同样记为今天已签
		a.rt.Store.UpdateWBCheckinDay(p.ID, checkinDay(time.Now()))
		return true, nil
	}
	return false, err
}

// wbCheckinAuthError 签到响应是否属于「凭证失效」（需要强制续期后重试）。
func wbCheckinAuthError(ae *wbAPIError) bool {
	return ae != nil && (ae.Status == http.StatusUnauthorized || ae.Code == 12153)
}

// checkinDay 签到的日期口径：北京时间（WorkBuddy 按 UTC+8 计一天，不随服务器时区漂移）
func checkinDay(now time.Time) string { return now.In(wbCheckinLoc).Format("2006-01-02") }

// wbCheckedInToday 当天是否已签到成功（本地记录，用于跳过后面的签到时点）
func wbCheckedInToday(cred *WorkBuddyCred, now time.Time) bool {
	return cred != nil && cred.LastCheckinDay == checkinDay(now)
}

// userResource 查询余额：企业账号走企业积分接口，其余走个人资源包接口。
// 查询成功后把快照写进凭证。
func (a *Adapter) userResource(p Provider) (*WBUsage, error) {
	_, cred := wbOf(p)
	if cred == nil {
		return nil, fmt.Errorf("provider 缺少 WorkBuddy 凭证")
	}
	var (
		u   *WBUsage
		err error
	)
	if cred.EnterpriseID != "" {
		u, err = a.enterpriseUsage(p)
	} else {
		u, err = a.personalUsage(p)
	}
	if err != nil {
		return nil, err
	}
	a.rt.Store.UpdateWBUsage(p.ID, u)
	return u, nil
}

// personalUsage 个人资源包余额：按当前有效套餐查询，周期口径优先、缺总额不凑数。
func (a *Adapter) personalUsage(p Provider) (*WBUsage, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	data, err := a.billingPost(p, "/v2/billing/meter/get-user-resource", body)
	if err != nil {
		return nil, err
	}
	resp, _ := data["Response"].(map[string]any)
	inner, _ := resp["Data"].(map[string]any)
	accounts, _ := inner["Accounts"].([]any)
	u := &WBUsage{CheckedAt: now}
	var total float64
	complete := true
	for _, aa := range accounts {
		raw, err := json.Marshal(aa)
		if err != nil {
			continue
		}
		var a struct {
			CapacityRemain      float64  `json:"CapacityRemain"`
			CapacitySize        *float64 `json:"CapacitySize"`
			CycleCapacityRemain float64  `json:"CycleCapacityRemain"`
			CycleCapacitySize   float64  `json:"CycleCapacitySize"`
			CycleCapacityUsed   float64  `json:"CycleCapacityUsed"`
		}
		if json.Unmarshal(raw, &a) != nil {
			continue
		}
		remain := a.CapacityRemain
		var size *float64
		switch {
		case a.CycleCapacitySize > 0: // 周期字段齐全：周期口径
			remain, size = a.CycleCapacityRemain, &a.CycleCapacitySize
		case a.CycleCapacityRemain > 0 || a.CycleCapacityUsed > 0: // 有周期用量无总额：现算
			remain = a.CycleCapacityRemain
			t := math.Max(0, remain) + math.Max(0, a.CycleCapacityUsed)
			size = &t
		default: // 无周期信息：总剩余口径
			size = a.CapacitySize
		}
		u.Remain += math.Max(0, remain)
		if size == nil || *size < 0 {
			complete = false
		} else {
			total += *size
		}
	}
	if complete { // 缺任一套餐总额时不输出 total/used，避免画出错误进度
		used := math.Max(0, total-u.Remain)
		u.Total, u.Used = &total, &used
	}
	return u, nil
}

// enterpriseUsage 企业成员积分：credit=周期已用、limitNum=周期总额度（-1 无限）。
// 积分保留小数。
func (a *Adapter) enterpriseUsage(p Provider) (*WBUsage, error) {
	data, err := a.billingPost(p, "/v2/billing/meter/get-enterprise-user-usage", []byte("{}"))
	if err != nil {
		return nil, err
	}
	credit, okC := data["credit"].(float64)
	limit, okL := data["limitNum"].(float64)
	if !okC || !okL || credit < 0 || (limit < 0 && limit != -1) {
		return nil, fmt.Errorf("企业积分响应缺少有效额度字段")
	}
	u := &WBUsage{CheckedAt: time.Now()}
	if limit == -1 {
		u.Unlimited = true
		u.Used = &credit
		return u, nil
	}
	remain := math.Max(0, limit-credit)
	u.Remain, u.Total, u.Used = remain, &limit, &credit
	return u, nil
}

// ── 定时签到 ──────────────────────────────────────────────────────────────

// checkinHour, checkinMinute 每日自动签到基线时点：每天 09:45（北京时间）
const checkinHour, checkinMinute = 9, 45

// wbCheckinLoc 签到日与签到时刻的固定口径：北京时间（不随服务器时区漂移）
var wbCheckinLoc = time.FixedZone("CST", 8*60*60)

// 自动签到的时间参数。测试可置 0 关闭等待，生产默认值见下。
var (
	// CheckinJitterMax 每日时点的随机抖动上限；抖动按日期固定，同一天重启不会改变时点。
	CheckinJitterMax = 15 * time.Minute
	// CheckinRetryDelay 首次失败后到唯一一次重试之间的间隔。
	CheckinRetryDelay = time.Minute
	// CheckinAccountGapMin/Max 多账号之间的随机间隔，避免同一毫秒批量签到。
	CheckinAccountGapMin = time.Minute
	CheckinAccountGapMax = 3 * time.Minute
)

// wbDailyJitter 按北京日期生成的固定抖动：同一天内无论重启多少次，签到时刻都一样。
func wbDailyJitter(day time.Time) time.Duration {
	if CheckinJitterMax <= 0 {
		return 0
	}
	y, m, d := day.In(wbCheckinLoc).Date()
	seed := uint64(y)<<32 | uint64(m)<<16 | uint64(d)
	r := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	seconds := int64(CheckinJitterMax / time.Second)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(r.Int64N(seconds+1)) * time.Second
}

// wbAccountGap 多账号签到之间的随机间隔。
func wbAccountGap() time.Duration {
	lo, hi := CheckinAccountGapMin, CheckinAccountGapMax
	if lo < 0 {
		lo = 0
	}
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rand.Int64N(int64(hi-lo)))
}

// wbNextCheckinAt 下一个自动签到时点：北京时间 09:45 + 当日固定抖动；今天已过则取
// 明天。返回值带北京时间时区，Sleep/Until 仍按绝对时刻计算。
func wbNextCheckinAt(now time.Time) time.Time {
	bj := now.In(wbCheckinLoc)
	at := func(day time.Time) time.Time {
		base := time.Date(day.Year(), day.Month(), day.Day(), checkinHour, checkinMinute, 0, 0, wbCheckinLoc)
		return base.Add(wbDailyJitter(base))
	}
	if t := at(bj); bj.Before(t) {
		return t
	}
	return at(bj.AddDate(0, 0, 1))
}

// checkinLoop 后台定时签到：每天 09:45（北京时间）+ 当日抖动跑一轮。
func (a *Adapter) checkinLoop() {
	for {
		time.Sleep(time.Until(wbNextCheckinAt(time.Now())))
		a.checkinRound(time.Now())
	}
}

// wbCheckinOutcome 签到结果的展示文案。
func wbCheckinOutcome(already bool) string {
	if already {
		return "今天已签到（幂等）"
	}
	return "签到成功"
}

// checkinRound 一轮签到：国内版个人账号（有凭证、非企业）签到并刷新余额快照。
// 当天已签过的账号（如管理员手动签过）不再请求签到接口，只刷新余额快照。国际版
// 不支持签到，企业账号由管理员手动查积分。
// 首次签到失败后等待 CheckinRetryDelay 重试一次；无论重试成败都到此为止（下一次是
// 明天的定时任务），避免失败时对上游连环重试。多账号之间留随机间隔。
func (a *Adapter) checkinRound(now time.Time) {
	var failed []Provider
	processed := 0
	for _, p := range a.rt.Store.AllProviders() {
		e, cred := wbOf(p)
		if e == nil || e.intl || cred == nil || cred.AccessToken == "" || cred.EnterpriseID != "" {
			continue
		}
		if processed > 0 {
			time.Sleep(wbAccountGap())
		}
		processed++
		if !wbCheckedInToday(cred, now) {
			already, err := a.checkin(p)
			entry := LogEntry{Level: "info", Kind: domain.KindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 自动签到：%s", p.Name, wbCheckinOutcome(already))}
			if err != nil {
				entry.Level = "warn"
				entry.Message = fmt.Sprintf("WorkBuddy %s 自动签到失败（稍后重试一次）", p.Name)
				entry.Detail = err.Error()
				failed = append(failed, p)
			}
			a.rt.Logf(entry)
		}
		if _, uerr := a.userResource(p); uerr != nil {
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 余额刷新失败", p.Name), Detail: uerr.Error()})
		}
	}
	if len(failed) == 0 {
		return
	}
	// 唯一一次重试：等一分钟再试；无论成败都不再重试。
	if CheckinRetryDelay > 0 {
		time.Sleep(CheckinRetryDelay)
	}
	for _, p := range failed {
		already, err := a.checkin(p)
		if err != nil {
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 自动签到重试失败（今天不再尝试）", p.Name),
				Detail:  err.Error()})
			continue
		}
		a.rt.Logf(LogEntry{Level: "info", Kind: domain.KindCheckin, ProviderID: p.ID,
			Message: fmt.Sprintf("WorkBuddy %s 自动签到重试成功：%s", p.Name, wbCheckinOutcome(already))})
		if _, uerr := a.userResource(p); uerr != nil {
			a.rt.Logf(LogEntry{Level: "warn", Kind: domain.KindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 余额刷新失败", p.Name), Detail: uerr.Error()})
		}
	}
}
