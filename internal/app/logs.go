package app

// maxLogs 异常日志最多保留的条数（保留在根包仅为兼容旧测试，实际实现见 internal/store）
const maxLogs = 500

// logFail 记一条异常日志。detail 会截断，避免把大段上游响应塞进状态文件。
func (s *Server) logFail(e LogEntry) {
	e.Detail = truncateRunes(e.Detail, 500)
	s.store.AddLog(&e)
}

// logSession 记一条会话建立日志（模型选择结果 + 理由）。
// 只记成功选定的那次，且失败重试导致的切换会另起一条，便于回溯"为什么走了这个上游"。
func (s *Server) logSession(e LogEntry) {
	e.Kind = kindSession
	e.Level = "info"
	s.store.AddLog(&e)
}
