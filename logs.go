package main

import (
	"time"
)

// maxLogs 异常日志最多保留的条数，超出时丢弃最旧的
const maxLogs = 500

// LogEntry 一次异常或失败记录。
// 只记失败与异常，成功请求走用量统计（Usage），不在这里重复记录。
type LogEntry struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`  // error | warn
	Kind       string    `json:"kind"`   // 失败类型，见下面 kind* 常量
	Status     int       `json:"status"` // 相关 HTTP 状态码：上游失败时为上游返回码，其余为返回给客户端的码
	KeyID      string    `json:"key_id,omitempty"`
	ProviderID string    `json:"provider_id,omitempty"`
	Model      string    `json:"model,omitempty"`
	Message    string    `json:"message"`          // 简要说明
	Detail     string    `json:"detail,omitempty"` // 上游错误详情（已截断）
}

// 失败类型
const (
	kindAuth        = "auth"         // 客户端 API Key 无效
	kindBadRequest  = "bad_request"  // 请求体不合法
	kindNoCandidate = "no_candidate" // 没有可用上游
	kindUpstream    = "upstream"     // 单个上游调用失败（可能已故障转移到下一个）
	kindAllFailed   = "all_failed"   // 所有上游都失败，返回 502
	kindAdminAuth   = "admin_auth"   // 后台令牌错误
	kindAdminLocked = "admin_locked" // 后台令牌因连续错误被锁定
)

// logFail 记一条异常日志。detail 会截断，避免把大段上游响应塞进状态文件。
func (s *Server) logFail(e LogEntry) {
	e.Detail = truncateRunes(e.Detail, 500)
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
