package main

import (
	"time"
)

// maxLogs 异常日志最多保留的条数，超出时丢弃最旧的
const maxLogs = 500

// LogEntry 一次异常、失败或会话建立记录。
type LogEntry struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`  // info | warn | error
	Kind       string    `json:"kind"`   // 记录类型，见下面 kind* 常量
	Status     int       `json:"status"` // 相关 HTTP 状态码：上游失败时为上游返回码，其余为返回给客户端的码
	KeyID      string    `json:"key_id,omitempty"`
	ProviderID string    `json:"provider_id,omitempty"`
	Model      string    `json:"model,omitempty"`
	Message    string    `json:"message"`          // 简要说明
	Detail     string    `json:"detail,omitempty"` // 上游错误详情（已截断）

	// 会话建立相关（kind = session）
	// 只在新会话首次选路、或会话中途重新选模型时记录；同一会话复用绑定不重复记。
	SessionID  string       `json:"session_id,omitempty"` // 客户端 X-Session-ID 原文
	Requested  string       `json:"requested,omitempty"`  // 请求体里的 model 字段
	Switched   bool         `json:"switched,omitempty"`   // true = 会话中途换了模型，false = 新会话建立
	Reason     string       `json:"reason,omitempty"`     // 选择该模型的理由（一句话）
	Candidates []string     `json:"candidates,omitempty"` // 候选顺序：Provider/模型（优先级）
	Skipped    []SkipReason `json:"skipped,omitempty"`    // 被剔除的条目及原因
}

// 记录类型
const (
	kindAuth        = "auth"         // 客户端 API Key 无效
	kindBadRequest  = "bad_request"  // 请求体不合法
	kindNoCandidate = "no_candidate" // 没有可用上游
	kindUpstream    = "upstream"     // 单个上游调用失败（可能已故障转移到下一个）
	kindAllFailed   = "all_failed"   // 所有上游都失败，返回 502
	kindAdminAuth   = "admin_auth"   // 后台令牌错误
	kindAdminLocked = "admin_locked" // 后台令牌因连续错误被锁定
	kindSession     = "session"      // 会话建立/切换绑定
)

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

// truncateRunes 按字符截断，避免切断多字节字符
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// AddLog 追加一条日志并落盘，超出上限时丢弃最旧的
func (s *Store) AddLog(e *LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = s.newID("l")
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Level == "" {
		e.Level = "error"
	}
	s.Logs = append(s.Logs, e)
	if len(s.Logs) > maxLogs {
		s.Logs = append([]*LogEntry(nil), s.Logs[len(s.Logs)-maxLogs:]...)
	}
	s.save()
}

// ClearLogs 清空所有异常日志
func (s *Store) ClearLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logs = nil
	s.save()
}
