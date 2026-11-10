package store

import "time"

// maxLogs 异常日志最多保留的条数，超出时丢弃最旧的
const maxLogs = 500

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
	s.saveHot()
}

// ClearLogs 清空所有异常日志
func (s *Store) ClearLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logs = nil
	s.saveHot()
}
