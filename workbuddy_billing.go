package main

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
// 计费头族比对话/控制面更窄：鉴权 + 账号身份即可；refresh token 只允许出现在续期
// 接口，签到/余额请求绝不携带。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// WBUsage 一次余额查询的安全快照（存进凭证，随状态文件落盘）
type WBUsage struct {
	Remain    float64   `json:"remain"`              // 剩余额度（个人=各套餐周期剩余之和；企业=max(0,limit-credit)）
	Total     *float64  `json:"total,omitempty"`     // 周期总额度；nil = 上游未暴露（不画进度）
	Used      *float64  `json:"used,omitempty"`      // 周期已用；nil = 同上
	Unlimited bool      `json:"unlimited,omitempty"` // 企业无限额度（limitNum=-1）
	CheckedAt time.Time `json:"checked_at"`
}

// wbCheckinBusinessCode 签到接口的通用业务拒绝码（所有非成功都是它）
const wbCheckinBusinessCode = 10001

// wbCheckinAlreadyMarker 上游「当天已签到」幂等成功的文案特征（上游只有这一种措辞族）
const wbCheckinAlreadyMarker = "已签到"

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

// wbBillingHeader 计费接口的请求头（比对话/控制面窄：鉴权 + 账号身份）
func wbBillingHeader(cred *WorkBuddyCred) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	wbAuthHeaders(h, cred)
	return h
}

// wbBillingPost 发一个计费 POST 并解开 {code, msg, data} 信封；code 非 0 时返回
// *wbAPIError。发之前按需懒续期（与对话路径一致）。
func (s *Server) wbBillingPost(p Provider, path string, body []byte) (map[string]any, error) {
	e, cred := wbOf(p)
	if e == nil || cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("未导入 WorkBuddy 凭证")
	}
	if cred.RefreshToken != "" && wbTokenExpiring(cred.expires()) {
		if nc, err := s.wbRefreshToken(p, false); err == nil {
			cred = nc
		}
	}
	req, err := http.NewRequest(http.MethodPost, wbEndpoint(p)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = wbBillingHeader(cred)
	cli, cerr := s.upstreamClient(p, 20*time.Second)
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
		return nil, fmt.Errorf("计费响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	if v.Code != nil && *v.Code != 0 {
		msg := v.Msg
		if msg == "" {
			msg = v.Message
		}
		return nil, &wbAPIError{Status: resp.StatusCode, Code: int64(*v.Code), Msg: msg}
	}
	var data map[string]any
	if len(v.Data) > 0 && json.Unmarshal(v.Data, &data) == nil {
		return data, nil
	}
	return map[string]any{}, nil
}

// wbCheckin 国内版个人账号每日签到。already=true 表示当天已签过（幂等成功）。
// 企业账号不走个人签到（上游拒绝），由调用方分流。
// 成功与「上游说已签过」都会把当天记为已签到，当天后续时点不再请求签到接口。
func (s *Server) wbCheckin(p Provider) (already bool, err error) {
	_, err = s.wbBillingPost(p, "/v2/billing/meter/daily-checkin", []byte("{}"))
	if err == nil {
		s.store.UpdateWBCheckinDay(p.ID, wbCheckinDay(time.Now()))
		return false, nil
	}
	var ae *wbAPIError
	if errors.As(err, &ae) && ae.Code == wbCheckinBusinessCode && strings.Contains(ae.Msg, wbCheckinAlreadyMarker) {
		// 上游说今天已签过（可能来自官方客户端）：同样记为今天已签
		s.store.UpdateWBCheckinDay(p.ID, wbCheckinDay(time.Now()))
		return true, nil
	}
	return false, err
}

// wbCheckinDay 签到的日期口径（服务器本地时间的年月日）
func wbCheckinDay(now time.Time) string { return now.Format("2006-01-02") }

// wbCheckedInToday 当天是否已签到成功（本地记录，用于跳过后面的签到时点）
func wbCheckedInToday(cred *WorkBuddyCred, now time.Time) bool {
	return cred != nil && cred.LastCheckinDay == wbCheckinDay(now)
}

// wbUserResource 查询余额：企业账号走企业积分接口，其余走个人资源包接口。
// 查询成功后把快照写进凭证。
func (s *Server) wbUserResource(p Provider) (*WBUsage, error) {
	_, cred := wbOf(p)
	if cred == nil {
		return nil, fmt.Errorf("provider 缺少 WorkBuddy 凭证")
	}
	var (
		u   *WBUsage
		err error
	)
	if cred.EnterpriseID != "" {
		u, err = s.wbEnterpriseUsage(p)
	} else {
		u, err = s.wbPersonalUsage(p)
	}
	if err != nil {
		return nil, err
	}
	s.store.UpdateWBUsage(p.ID, u)
	return u, nil
}

// wbPersonalUsage 个人资源包余额：按当前有效套餐查询，周期口径优先、缺总额不凑数。
func (s *Server) wbPersonalUsage(p Provider) (*WBUsage, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	data, err := s.wbBillingPost(p, "/v2/billing/meter/get-user-resource", body)
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

// wbEnterpriseUsage 企业成员积分：credit=周期已用、limitNum=周期总额度（-1 无限）。
// 积分保留小数。
func (s *Server) wbEnterpriseUsage(p Provider) (*WBUsage, error) {
	data, err := s.wbBillingPost(p, "/v2/billing/meter/get-enterprise-user-usage", []byte("{}"))
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

// wbCheckinHour 每日自动签到时点：每天 09:00 一次（服务器本地时间）
const wbCheckinHour = 9

// wbNextCheckinAt 下一个自动签到时点（本地时间 09:00；今天已过则取明天 09:00）
func wbNextCheckinAt(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), wbCheckinHour, 0, 0, 0, now.Location())
	if now.Before(t) {
		return t
	}
	return t.AddDate(0, 0, 1)
}

// wbCheckinLoop 后台定时签到：每天 09:00 跑一轮。
func (s *Server) wbCheckinLoop() {
	for {
		time.Sleep(time.Until(wbNextCheckinAt(time.Now())))
		s.wbCheckinRound(time.Now())
	}
}

// wbCheckinRound 一轮签到：国内版个人账号（有凭证、非企业）签到并刷新余额快照。
// 当天已签过的账号（如管理员手动签过）不再请求签到接口，只刷新余额快照。国际版
// 不支持签到，企业账号由管理员手动查积分。失败只记日志，等次日再来。
func (s *Server) wbCheckinRound(now time.Time) {
	for _, p := range s.store.AllProviders() {
		e, cred := wbOf(p)
		if e == nil || e.intl || cred == nil || cred.AccessToken == "" || cred.EnterpriseID != "" {
			continue
		}
		if !wbCheckedInToday(cred, now) {
			already, err := s.wbCheckin(p)
			entry := LogEntry{Level: "info", Kind: kindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 自动签到：%s", p.Name,
					map[bool]string{true: "今天已签到（幂等）", false: "签到成功"}[already])}
			if err != nil {
				entry.Level, entry.Message = "warn",
					fmt.Sprintf("WorkBuddy %s 自动签到失败", p.Name)
				entry.Detail = err.Error()
			}
			s.logFail(entry)
		}
		if _, uerr := s.wbUserResource(p); uerr != nil {
			s.logFail(LogEntry{Level: "warn", Kind: kindCheckin, ProviderID: p.ID,
				Message: fmt.Sprintf("WorkBuddy %s 余额刷新失败", p.Name), Detail: uerr.Error()})
		}
	}
}
