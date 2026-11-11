package app

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strings"
)

// derivedSessionID 为未携带 X-Session-ID 的请求派生会话 ID：取客户端 IP + 源端口
// （r.RemoteAddr）算 md5。同一条 TCP 连接（keep-alive 复用）内保持不变，客户端
// 新开连接即视为新会话。RemoteAddr 为空时返回空串，表示不做会话绑定。
func derivedSessionID(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return ""
	}
	sum := md5.Sum([]byte(addr))
	return hex.EncodeToString(sum[:])
}
