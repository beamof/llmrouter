package wire

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// wbUUID7 生成 UUIDv7 原始字节：48bit 毫秒时间戳 + 版本 7 + 随机位（crypto/rand）。
// 适配器用非加密随机源；这里换 crypto/rand 更稳妥，上游不校验随机性。
func uuid7() [16]byte {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	rand.Read(b[6:])
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return b
}

// wbMessageID 32 位小写 hex（X-Request-ID / X-Trace-ID 等）
func MessageID() string {
	b := uuid7()
	return hex.EncodeToString(b[:])
}

// wbConversationID 连字符 8-4-4-4-12 形式（X-Conversation-ID）
func ConversationID() string {
	b := uuid7()
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// wbSpanID 16 位小写 hex（traceparent / b3）
func SpanID() string {
	b := uuid7()
	return hex.EncodeToString(b[:8])
}
