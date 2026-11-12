package app

import (
	"crypto/md5"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

// derivedSessionID 为未携带 X-Session-ID 的请求派生会话 ID：取客户端 IP 算
// UUID v3（md5(IP)，RFC 4122 格式）。同一 IP 的请求共用一个固定 UUID，不论
// 源端口与 TCP 连接如何变化；不同 IP 得到不同 UUID。RemoteAddr 为空时返回
// 空串，表示不做会话绑定。
func derivedSessionID(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return ""
	}
	ip := addr
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		ip = host
	}
	return uuidV3(md5.Sum([]byte(ip)))
}

// uuidV3 把 16 字节 md5 摘要写成 RFC 4122 UUID 字符串（版本 3，变体 10xx）。
func uuidV3(sum [16]byte) string {
	sum[6] = (sum[6] & 0x0f) | 0x30 // version 3
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant RFC 4122
	hexStr := hex.EncodeToString(sum[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}
