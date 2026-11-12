package workbuddy

// WorkBuddy 计费测试：签到幂等语义、个人资源包聚合、企业积分、handler 分流
//（国内/国际/企业）、余额快照落盘与自动签到时点计算。

import (
	"testing"
	"time"
)

// wbBillingUpstream 起一个带计费端点的伪造上游：checkin 控制签到行为，personal /
// enterprise 分别是两个余额端点的响应；返回各端点命中计数。

func TestWBNextCheckinAt(t *testing.T) {
	old := CheckinJitterMax
	CheckinJitterMax = 0 // 本测试只验证基线时点
	t.Cleanup(func() { CheckinJitterMax = old })

	loc := time.FixedZone("t", 8*3600)
	day := func(h, m int) time.Time { return time.Date(2026, 11, 8, h, m, 0, 0, loc) }
	cases := []struct {
		now, want time.Time
	}{
		{day(6, 0), day(9, 45)},                    // 未到 9:45：今天 9:45
		{day(9, 45), day(9, 45).AddDate(0, 0, 1)},  // 9:45 整已到：明天 9:45
		{day(9, 44), day(9, 45)},                   // 差一分钟仍取今天
		{day(12, 30), day(9, 45).AddDate(0, 0, 1)}, //
		{day(21, 0), day(9, 45).AddDate(0, 0, 1)},  // 每天只有 9:45 一个时点
		{day(23, 59), day(9, 45).AddDate(0, 0, 1)}, //
		// 服务器在 UTC 时也按北京时间 09:45 排程：UTC 01:00 = 北京 09:00
		{time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC), day(9, 45)},
	}
	for _, c := range cases {
		if got := wbNextCheckinAt(c.now); !got.Equal(c.want) {
			t.Fatalf("wbNextCheckinAt(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestWBCheckinDayUsesBeijingDate(t *testing.T) {
	// UTC 2026-11-07 17:00 = 北京 2026-11-08 01:00，日期必须按北京时间算
	now := time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC)
	if got := checkinDay(now); got != "2026-11-08" {
		t.Fatalf("checkinDay(%v) = %q, want 2026-11-08（北京时间）", now, got)
	}
}

func TestWBCheckinJitterStablePerDay(t *testing.T) {
	old := CheckinJitterMax
	CheckinJitterMax = 15 * time.Minute
	t.Cleanup(func() { CheckinJitterMax = old })

	base := time.Date(2026, 11, 8, checkinHour, checkinMinute, 0, 0, wbCheckinLoc)
	first := wbNextCheckinAt(time.Date(2026, 11, 8, 0, 0, 0, 0, wbCheckinLoc))
	// 同一天重启（哪怕晚几个小时）必须得到同一个时点
	if again := wbNextCheckinAt(time.Date(2026, 11, 8, 5, 0, 0, 0, wbCheckinLoc)); !again.Equal(first) {
		t.Fatalf("同一天重启后的时点应稳定: %v vs %v", first, again)
	}
	if off := first.Sub(base); off < 0 || off > CheckinJitterMax {
		t.Fatalf("抖动 %v 超出 [0, %v]", off, CheckinJitterMax)
	}
	// 今天抖动后的时点已过：取明天（同样带固定抖动）
	next := wbNextCheckinAt(base.Add(CheckinJitterMax + time.Second))
	nextBase := time.Date(2026, 11, 9, checkinHour, checkinMinute, 0, 0, wbCheckinLoc)
	if off := next.Sub(nextBase); off < 0 || off > CheckinJitterMax {
		t.Fatalf("次日抖动 %v 超出 [0, %v]", off, CheckinJitterMax)
	}
}

func TestWBAccountGap(t *testing.T) {
	oldMin, oldMax := CheckinAccountGapMin, CheckinAccountGapMax
	t.Cleanup(func() { CheckinAccountGapMin, CheckinAccountGapMax = oldMin, oldMax })

	CheckinAccountGapMin, CheckinAccountGapMax = 0, 0
	if got := wbAccountGap(); got != 0 {
		t.Fatalf("关闭间隔时应为 0，得到 %v", got)
	}
	CheckinAccountGapMin, CheckinAccountGapMax = time.Minute, 3*time.Minute
	for i := 0; i < 100; i++ {
		if got := wbAccountGap(); got < time.Minute || got > 3*time.Minute {
			t.Fatalf("账号间隔 %v 超出 [1m, 3m]", got)
		}
	}
}
