package app

// WorkBuddy 计费测试：签到幂等语义、个人资源包聚合、企业积分、handler 分流
//（国内/国际/企业）、余额快照落盘与自动签到时点计算。

import (
	"encoding/json"
	"fmt"
	"io"
	"llmrouter/internal/upstream/workbuddy"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wbBillingUpstream 起一个带计费端点的伪造上游：checkin 控制签到行为，personal /
// enterprise 分别是两个余额端点的响应；返回各端点命中计数。

func wbBillingUpstream(t *testing.T, checkin func(w http.ResponseWriter), personal, enterprise string) (*httptest.Server, *map[string]*atomic.Int32) {
	t.Helper()
	hits := map[string]*atomic.Int32{
		"daily-checkin": {}, "get-user-resource": {}, "get-enterprise-user-usage": {}, "refresh": {},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		hits["daily-checkin"].Add(1)
		checkin(w)
	})
	mux.HandleFunc("POST /v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		hits["refresh"].Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`)
	})
	mux.HandleFunc("POST /v2/billing/meter/get-user-resource", func(w http.ResponseWriter, r *http.Request) {
		hits["get-user-resource"].Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, personal)
	})
	mux.HandleFunc("POST /v2/billing/meter/get-enterprise-user-usage", func(w http.ResponseWriter, r *http.Request) {
		hits["get-enterprise-user-usage"].Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, enterprise)
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)
	return up, &hits
}

func TestWBCheckinSemantics(t *testing.T) {
	ok := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}
	already := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":10001,"msg":"今天已签到，请明天再来"}`)
	}
	rejected := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":10001,"msg":"企业账号不支持该操作"}`)
	}
	for name, tc := range map[string]struct {
		checkin     func(w http.ResponseWriter)
		wantAlready bool
		wantErr     string
	}{
		"签到成功":      {ok, false, ""},
		"当天已签过幂等":   {already, true, ""},
		"真实失败按错误返回": {rejected, false, "企业账号不支持该操作"},
	} {
		t.Run(name, func(t *testing.T) {
			up, _ := wbBillingUpstream(t, tc.checkin, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
			s, _ := newTestServer(t)
			p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
			got, err := s.wbAdapter().Checkin(*p)
			if got != tc.wantAlready {
				t.Fatalf("already = %v, want %v（err=%v）", got, tc.wantAlready, err)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("应报错含 %q: %v", tc.wantErr, err)
			}
		})
	}
}

// HTTP 层 500 但信封里没业务码（或不是 JSON）时不能当成功吞掉

func TestWBCheckinBareHTTPErrorIsFailure(t *testing.T) {
	for name, body := range map[string]string{
		"JSON 缺 code": `{"message":"upstream panic"}`,
		"HTML 风控页":    `<html><body>blocked</body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			up, _ := wbBillingUpstream(t, func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(500)
				fmt.Fprint(w, body)
			}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
			s, _ := newTestServer(t)
			p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
			already, err := s.wbAdapter().Checkin(*p)
			if err == nil || already {
				t.Fatalf("HTTP 500 应报错: already=%v err=%v", already, err)
			}
			if !strings.Contains(err.Error(), "HTTP 500") {
				t.Fatalf("错误应带状态码: %v", err)
			}
			if st, _ := s.store.Provider(p.ID); st.WB.LastCheckinDay != "" {
				t.Fatalf("失败不应记录签到日期，得到 %q", st.WB.LastCheckinDay)
			}
		})
	}
}

// 当天已签过（手动签过或重启后重跑）不再请求签到接口，只刷新余额快照

func TestWBCheckinRoundSkipsSameDayRepeat(t *testing.T) {
	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	now := time.Now()
	s.wbAdapter().CheckinRound(now)
	s.wbAdapter().CheckinRound(now) // 同日重跑：已签过，只刷余额
	if got := (*hits)["daily-checkin"].Load(); got != 1 {
		t.Fatalf("当天应只签到一次，实际 %d 次", got)
	}
	if got := (*hits)["get-user-resource"].Load(); got != 2 {
		t.Fatalf("每轮都应刷新余额，实际 %d 次", got)
	}
	if st, _ := s.store.Provider(p.ID); st.WB.LastCheckinDay != workbuddy.CheckinDay(now) {
		t.Fatalf("签到日期应按北京时间落盘，得到 %q", st.WB.LastCheckinDay)
	}
}

// 首次签到失败后一分钟只重试一次；再失败即结束，不连环重试

func TestWBCheckinRoundRetriesOnceAfterFailure(t *testing.T) {
	old := workbuddy.CheckinRetryDelay
	workbuddy.CheckinRetryDelay = 0 // 测试不等一分钟
	t.Cleanup(func() { workbuddy.CheckinRetryDelay = old })

	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":10001,"msg":"企业账号不支持该操作"}`)
	}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	now := time.Now()
	s.wbAdapter().CheckinRound(now)
	if got := (*hits)["daily-checkin"].Load(); got != 2 {
		t.Fatalf("失败后应重试一次（共 2 次请求），实际 %d 次", got)
	}
	if st, _ := s.store.Provider(p.ID); st.WB.LastCheckinDay != "" {
		t.Fatalf("失败不应记录签到日期，得到 %q", st.WB.LastCheckinDay)
	}
}

// 首次失败、一分钟后重试成功：记为当天已签，同日不再请求签到接口

func TestWBCheckinRoundRetrySucceeds(t *testing.T) {
	old := workbuddy.CheckinRetryDelay
	workbuddy.CheckinRetryDelay = 0
	t.Cleanup(func() { workbuddy.CheckinRetryDelay = old })

	var calls atomic.Int32
	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"code":10001,"msg":"服务繁忙"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	now := time.Now()
	s.wbAdapter().CheckinRound(now)
	if got := (*hits)["daily-checkin"].Load(); got != 2 {
		t.Fatalf("首次 500 后应重试成功（共 2 次），实际 %d 次", got)
	}
	if st, _ := s.store.Provider(p.ID); st.WB.LastCheckinDay != workbuddy.CheckinDay(now) {
		t.Fatalf("重试成功应记录签到日期，得到 %q", st.WB.LastCheckinDay)
	}
	s.wbAdapter().CheckinRound(now) // 同日再跑：已签过，不再请求签到
	if got := (*hits)["daily-checkin"].Load(); got != 2 {
		t.Fatalf("重试成功当天不应再签到，实际 %d 次", got)
	}
}

// 签到遇 401/12153 时强制续期一次并重试（走手动签到同一条 checkin 路径）

func TestWBCheckinRefreshExpiredTokenAndRetries(t *testing.T) {
	var calls atomic.Int32
	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":12153,"msg":"token expired"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: workbuddy.TimePtr(time.Now().Add(time.Hour)),
	})
	already, err := s.wbAdapter().Checkin(*p)
	if err != nil || already {
		t.Fatalf("续期重试后应签到成功: already=%v err=%v", already, err)
	}
	if got := (*hits)["refresh"].Load(); got != 1 {
		t.Fatalf("凭证失效应强制续期一次，实际 %d 次", got)
	}
	if got := (*hits)["daily-checkin"].Load(); got != 2 {
		t.Fatalf("续期后应重试签到（共 2 次），实际 %d 次", got)
	}
	if st, _ := s.store.Provider(p.ID); st.WB == nil || st.WB.AccessToken != "at-new" {
		t.Fatalf("续期结果应回写凭证: %#v", st.WB)
	}
}

// 上游回「今天已签到」（如官方客户端已签）同样算当天已签，同日重跑不再请求

func TestWBCheckinRoundHonorsUpstreamAlready(t *testing.T) {
	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":10001,"msg":"今天已签到，请明天再来"}`)
	}, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	now := time.Now()
	s.wbAdapter().CheckinRound(now)
	s.wbAdapter().CheckinRound(now)
	if got := (*hits)["daily-checkin"].Load(); got != 1 {
		t.Fatalf("上游幂等命中后当天不应再请求签到，实际 %d 次", got)
	}
}

func TestWBPersonalUsageAggregation(t *testing.T) {
	// 周期口径优先、缺周期回退总剩余、负值截零、缺总额不凑 total（对齐 ccLoad 实测口径）
	mixed := `{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800,"CapacityRemain":9999},
		{"CycleCapacitySize":0,"CycleCapacityRemain":300,"CycleCapacityUsed":700,"CapacityRemain":9999},
		{"CycleCapacitySize":0,"CycleCapacityRemain":0,"CycleCapacityUsed":0,"CapacityRemain":100},
		{"CycleCapacitySize":100,"CycleCapacityRemain":-1,"CycleCapacityUsed":101,"CapacityRemain":500}
	]}}}}`
	up, _ := wbBillingUpstream(t, func(w http.ResponseWriter) {}, mixed, `{"code":0,"data":{}}`)
	s, _ := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0", &WorkBuddyCred{UID: "u", AccessToken: "at-1"})
	u, err := s.wbAdapter().UserResource(*p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Remain != 1600 || u.Total != nil || u.Used != nil {
		t.Fatalf("混合套餐聚合 = remain %v total %v used %v, want 1600 / nil / nil", u.Remain, u.Total, u.Used)
	}

	complete := `{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":500,"CycleCapacityRemain":484.5,"CapacitySize":9999},
		{"CapacitySize":1500,"CapacityRemain":1400}
	]}}}}`
	up2, _ := wbBillingUpstream(t, func(w http.ResponseWriter) {}, complete, `{"code":0,"data":{}}`)
	p2, _ := addWBProviderModel(t, s, up2.URL, "glm-5.0", &WorkBuddyCred{UID: "u2", AccessToken: "at-1"})
	u2, err := s.wbAdapter().UserResource(*p2)
	if err != nil {
		t.Fatal(err)
	}
	if u2.Remain != 1884.5 || u2.Total == nil || *u2.Total != 2000 || u2.Used == nil || *u2.Used != 115.5 {
		t.Fatalf("完整套餐聚合 = %+v, want remain 1884.5 total 2000 used 115.5", u2)
	}
}

func TestWBEnterpriseUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		remain  float64
		unlimit bool
		wantErr bool
	}{
		"有限额度保留小数": {`{"code":0,"data":{"credit":104.35,"limitNum":2000}}`, 1895.65, false, false},
		"用尽截零":     {`{"code":0,"data":{"credit":2001.5,"limitNum":2000}}`, 0, false, false},
		"无限额度":     {`{"code":0,"data":{"credit":123.45,"limitNum":-1}}`, 0, true, false},
		"缺字段报错":    {`{"code":0,"data":{"limitNum":2000}}`, 0, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			up, _ := wbBillingUpstream(t, func(w http.ResponseWriter) {}, `{"code":0,"data":{}}`, tc.body)
			s, _ := newTestServer(t)
			p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0",
				&WorkBuddyCred{UID: "u", AccessToken: "at-1", EnterpriseID: "ent-1"})
			u, err := s.wbAdapter().UserResource(*p)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应报错")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.Unlimited != tc.unlimit || u.Remain != tc.remain {
				t.Fatalf("企业积分 = %+v, want remain %v unlimited %v", u, tc.remain, tc.unlimit)
			}
			if !tc.unlimit && (u.Total == nil || *u.Total != 2000 || u.Used == nil) {
				t.Fatalf("有限额度应带 total/used: %+v", u)
			}
		})
	}
}

func TestWBCheckinHandlerRouting(t *testing.T) {
	var checkinBody, resourceBody map[string]any
	var checkinHdr http.Header
	up, hits := wbBillingUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacitySize":2000,"CapacityRemain":1600}]}}}}`,
		`{"code":0,"data":{"credit":10,"limitNum":2000}}`)
	mux := up.Config.Handler.(*http.ServeMux) // 包一层捕获请求形状后转给计费 mux
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &checkinBody)
			checkinHdr = r.Header.Clone()
		case "/v2/billing/meter/get-user-resource":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &resourceBody)
		}
		mux.ServeHTTP(w, r)
	}))
	defer up2.Close()
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up2.URL, "glm-5.0", &WorkBuddyCred{UID: "uid-9", AccessToken: "at-1"})

	// 个人国内版：签到 + 刷新余额，快照落盘
	resp, body := postJSON(t, app, "/api/providers/"+p.ID+"/wb-checkin", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("签到应成功: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"already_checked_in":false`) || !strings.Contains(body, `"remain":1600`) {
		t.Fatalf("签到响应应含幂等标记与余额: %s", body)
	}
	if (*hits)["daily-checkin"].Load() != 1 || (*hits)["get-user-resource"].Load() != 1 {
		t.Fatal("个人国内版应各调一次签到与余额接口")
	}
	if len(checkinBody) != 0 {
		t.Fatalf("签到体应为空 JSON: %v", checkinBody)
	}
	if checkinHdr.Get("Authorization") != "Bearer at-1" || checkinHdr.Get("X-User-Id") != "uid-9" || checkinHdr.Get("X-Refresh-Token") != "" {
		t.Fatalf("计费请求头不对: %v", checkinHdr)
	}
	// 计费请求也要带官方客户端来源指纹，否则容易被上游风控当成脚本
	if checkinHdr.Get("User-Agent") != workbuddy.CLIUserAgent() ||
		checkinHdr.Get("Origin") != "https://www.codebuddy.cn" ||
		checkinHdr.Get("Referer") != "https://www.codebuddy.cn/" ||
		checkinHdr.Get("X-Requested-With") != "XMLHttpRequest" ||
		checkinHdr.Get("X-CodeBuddy-Request") != "1" {
		t.Fatalf("计费请求应带官方客户端指纹: %v", checkinHdr)
	}
	if resourceBody["ProductCode"] != "p_tcaca" || resourceBody["PageNumber"] != float64(1) || resourceBody["PageSize"] != float64(100) {
		t.Fatalf("余额请求筛选条件不对: %v", resourceBody)
	}
	if st, _ := s.store.Provider(p.ID); st.WB == nil || st.WB.Usage == nil || st.WB.Usage.Remain != 1600 {
		t.Fatalf("余额快照应写入凭证: %#v", st.WB)
	}

	// 国际版：直接 400，不触达计费端点
	pIntl := &Provider{Name: "wb-intl", Kind: "workbuddy-intl", BaseURL: up2.URL + "/v2", WB: &WorkBuddyCred{UID: "i", AccessToken: "at-1"}}
	s.store.UpsertProvider(pIntl)
	resp, body = postJSON(t, app, "/api/providers/"+pIntl.ID+"/wb-checkin", `{}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "国际版") {
		t.Fatalf("国际版签到应 400: %d %s", resp.StatusCode, body)
	}

	// 企业账号：不签个人到，改查企业积分（credit 10 / limit 2000 → 剩 1990）
	pEnt := &Provider{Name: "wb-ent", Kind: "workbuddy", BaseURL: up2.URL + "/v2",
		WB: &WorkBuddyCred{UID: "e", AccessToken: "at-1", EnterpriseID: "ent-9"}}
	s.store.UpsertProvider(pEnt)
	resp, body = postJSON(t, app, "/api/providers/"+pEnt.ID+"/wb-checkin", `{}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "企业账号") || !strings.Contains(body, `"remain":1990`) {
		t.Fatalf("企业签到应走积分刷新: %d %s", resp.StatusCode, body)
	}
	if (*hits)["daily-checkin"].Load() != 1 { // 仍是个人那次，企业没有再签
		t.Fatalf("企业账号不应调用个人签到: %d", (*hits)["daily-checkin"].Load())
	}
}

func TestWBUsageHandler(t *testing.T) {
	up, _ := wbBillingUpstream(t, func(w http.ResponseWriter) {}, `{"code":0,"data":{}}`,
		`{"code":0,"data":{"credit":10,"limitNum":-1}}`)
	s, app := newTestServer(t)
	p, _ := addWBProviderModel(t, s, up.URL, "glm-5.0",
		&WorkBuddyCred{UID: "u", AccessToken: "at-1", EnterpriseID: "ent-1"})
	resp, body := postJSON(t, app, "/api/providers/"+p.ID+"/wb-usage", `{}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"unlimited":true`) {
		t.Fatalf("余额查询应返回无限额度: %d %s", resp.StatusCode, body)
	}
	if st, _ := s.store.Provider(p.ID); st.WB.Usage == nil || !st.WB.Usage.Unlimited {
		t.Fatalf("快照应落盘: %#v", st.WB.Usage)
	}
}
