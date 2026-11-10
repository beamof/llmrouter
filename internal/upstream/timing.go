package upstream

import (
	"io"
	"time"

	"llmrouter/internal/domain"
)

// Timing 记录一次上游调用的计时点：发起时刻与首个响应字节到达时刻。
// 首字节（TTFT）从发起请求算起，包含排队、预填充与上游出字延迟。
type Timing struct {
	Start time.Time
	first time.Time // 零值 = 还没收到任何响应字节
}

// NewTiming 开始计时。
func NewTiming() *Timing { return &Timing{Start: time.Now()} }

// MarkFirst 记录首个响应字节到达时刻（只记一次）。
func (t *Timing) MarkFirst() {
	if t.first.IsZero() {
		t.first = time.Now()
	}
}

// Sample 合成一次调用的用量与耗时。流式的生成耗时从首字节算到流结束（排除排队与
// 预填充），非流式只有端到端耗时；两者都由 TokPerSec 统一换算。
func (t *Timing) Sample(u domain.Usage, stream bool) domain.CallSample {
	end := time.Now()
	sm := domain.CallSample{Usage: u, Total: end.Sub(t.Start), Stream: stream}
	if !t.first.IsZero() {
		sm.TTFT = t.first.Sub(t.Start)
		if stream {
			sm.Gen = end.Sub(t.first)
		}
	}
	if sm.Gen <= 0 {
		sm.Gen = sm.Total
	}
	return sm
}

// FirstByteReader 在首次从上游读到字节时给 Timing 打点，不改变字节流。
type FirstByteReader struct {
	R io.Reader
	T *Timing
}

func (f *FirstByteReader) Read(p []byte) (int, error) {
	n, err := f.R.Read(p)
	if n > 0 {
		f.T.MarkFirst()
	}
	return n, err
}
