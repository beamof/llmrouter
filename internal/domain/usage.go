package domain

import "time"

// CallSample 一次调用的用量 + 吞吐计时。
type CallSample struct {
	Usage
	Stream bool          // 是否流式响应（决定 TTFT 是否有意义）
	TTFT   time.Duration // 首字节耗时
	Gen    time.Duration // 生成耗时
	Total  time.Duration // 端到端耗时

	// 思考等级：EffortDeclared 是客户端请求体里的声明值，EffortSent 是实际写进
	// 出站请求的档位（适配层解析后覆盖；标准上游字节透传，两者相同）
	EffortDeclared string
	EffortSent     string
}

// TokPerSec 输出速度；没有输出 token 或耗时时返回 0
func (u CallSample) TokPerSec() float64 {
	if u.Completion <= 0 || u.Gen <= 0 {
		return 0
	}
	return float64(u.Completion) / u.Gen.Seconds()
}

// Apply 用本次计时聚合结果更新用量，同时保留适配层已写入的思考等级
// （Sample 只负责 usage/计时，整体赋值会抹掉档位信息）。
func (u *CallSample) Apply(s CallSample) {
	s.EffortDeclared, s.EffortSent = u.EffortDeclared, u.EffortSent
	*u = s
}
