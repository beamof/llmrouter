package wire

import (
	"io"
	"net/http"
)

// FlushWriter 每写一次就 Flush，保证 SSE 分片及时到达客户端。
type FlushWriter struct{ W http.ResponseWriter }

func (f FlushWriter) Write(p []byte) (int, error) {
	n, err := f.W.Write(p)
	if fl, ok := f.W.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// sseSniffer 透传 SSE 的同时保留一份滚动尾部（不改任何字节），流结束后从中提取 usage
type Sniffer struct {
	W    io.Writer
	Tail []byte
}

func (s *Sniffer) Write(p []byte) (int, error) {
	n, err := s.W.Write(p)
	s.Tail = append(s.Tail, p[:n]...)
	if len(s.Tail) > 8192 {
		s.Tail = s.Tail[len(s.Tail)-8192:]
	}
	return n, err
}
