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
	}
	for _, c := range cases {
		if got := wbNextCheckinAt(c.now); !got.Equal(c.want) {
			t.Fatalf("wbNextCheckinAt(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}
