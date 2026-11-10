package domain

import "time"

// MutedUntil 该模型条目是否因上游 429 处于自动静音期
func MutedUntil(m *Model, now time.Time) bool {
	return m != nil && !m.MutedUntil.IsZero() && now.Before(m.MutedUntil)
}

// MutedNow 当前时间是否落在任一每日静音时段内（支持跨零点）
func MutedNow(ws []MuteWindow, now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()
	for _, w := range ws {
		f, err1 := time.Parse("15:04", w.From)
		t, err2 := time.Parse("15:04", w.To)
		if err1 != nil || err2 != nil || f.Equal(t) {
			continue
		}
		fm, tm := f.Hour()*60+f.Minute(), t.Hour()*60+t.Minute()
		if fm < tm {
			if cur >= fm && cur < tm {
				return true
			}
		} else if cur >= fm || cur < tm { // 跨零点时段
			return true
		}
	}
	return false
}
