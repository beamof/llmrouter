package geminiapp

// 手写的 protobuf 线格式子集与 gRPC 分帧。上游没有公开的 .proto，这里只做
// 编解码：出站按字段号拼字节，入站保留字段原文供上层按号取值，不做反射。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// maxMessageBytes 单条 protobuf 消息的字节上限
const maxMessageBytes = 16 << 20

const (
	maxProtoFields = 100000
	maxProtoDepth  = 32
	maxFieldNumber = 0x1fffffff
)

var errProto = errors.New("protobuf 消息格式非法")

// pbField 解析出的一个 protobuf 字段。Wire 是线类型（0 varint、1 定长 64 位、
// 2 长度分隔、5 定长 32 位）；Payload 是 varint 的原始字节，或长度分隔字段的内容。
type pbField struct {
	Number  int
	Wire    int
	Payload []byte
}

func pbPutVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// pbVarint 序列化一个 varint 字段
func pbVarint(number int, v uint64) []byte {
	return pbPutVarint(pbPutVarint(nil, uint64(number)<<3), v)
}

// pbBytes 序列化一个长度分隔字段
func pbBytes(number int, b []byte) []byte {
	dst := pbPutVarint(nil, uint64(number)<<3|2)
	dst = pbPutVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func pbString(number int, s string) []byte { return pbBytes(number, []byte(s)) }

func pbReadVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 { // 第 10 字节只能剩最高 1 位
			return 0, 0, errProto
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errProto
}

// pbParse 按字段顺序解出全部字段；嵌套的旧式 group 作为不透明内容整体保留。
func pbParse(b []byte) ([]pbField, error) {
	if len(b) > maxMessageBytes {
		return nil, errProto
	}
	fields := make([]pbField, 0, 8)
	count := 0
	var walk func(pos, group, depth int) (int, error)
	walk = func(pos, group, depth int) (int, error) {
		if depth > maxProtoDepth {
			return 0, errProto
		}
		for pos < len(b) {
			if count++; count > maxProtoFields {
				return 0, errProto
			}
			key, n, err := pbReadVarint(b[pos:])
			if err != nil {
				return 0, err
			}
			pos += n
			number, wire := int(key>>3), int(key&7)
			if number < 1 || number > maxFieldNumber {
				return 0, errProto
			}
			if wire == 4 { // end group
				if number != group {
					return 0, errProto
				}
				return pos, nil
			}
			payloadStart := pos
			switch wire {
			case 0:
				_, n, err := pbReadVarint(b[pos:])
				if err != nil {
					return 0, err
				}
				pos += n
			case 1:
				pos += 8
			case 5:
				pos += 4
			case 2:
				size, n, err := pbReadVarint(b[pos:])
				if err != nil {
					return 0, err
				}
				pos += n
				if size > maxMessageBytes || pos+int(size) > len(b) {
					return 0, errProto
				}
				payloadStart = pos
				pos += int(size)
			case 3:
				end, err := walk(pos, number, depth+1)
				if err != nil {
					return 0, err
				}
				pos = end
			default:
				return 0, errProto
			}
			if pos > len(b) {
				return 0, errProto
			}
			fields = append(fields, pbField{Number: number, Wire: wire, Payload: b[payloadStart:pos]})
		}
		if group != 0 { // 没等到 end group
			return 0, errProto
		}
		return pos, nil
	}
	if _, err := walk(0, 0, 0); err != nil {
		return nil, err
	}
	return fields, nil
}

// pbGet 取首个该号字段
func pbGet(fields []pbField, number int) (pbField, bool) {
	for _, f := range fields {
		if f.Number == number {
			return f, true
		}
	}
	return pbField{}, false
}

// pbUint 取 varint 字段的值（字段缺失或线类型不符时 ok = false）
func pbUint(fields []pbField, number int) (uint64, bool) {
	f, ok := pbGet(fields, number)
	if !ok || f.Wire != 0 {
		return 0, false
	}
	v, _, err := pbReadVarint(f.Payload)
	if err != nil || v > 0x7fffffff {
		return 0, false
	}
	return v, true
}

// pbText 取长度分隔字段的 UTF-8 文本（字段缺失时 ok = false）
func pbText(fields []pbField, number int) (string, bool) {
	f, ok := pbGet(fields, number)
	if !ok || f.Wire != 2 {
		return "", false
	}
	return string(f.Payload), true
}

// pbFloat 取 32 位浮点字段（IEEE754 位模式）
func pbFloat(fields []pbField, number int) (float32, bool) {
	f, ok := pbGet(fields, number)
	if !ok || f.Wire != 5 || len(f.Payload) != 4 {
		return 0, false
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(f.Payload)), true
}

// pbTimestamp protobuf Timestamp（秒 + 纳秒）→ time.Time；缺秒数时 ok = false
func pbTimestamp(fields []pbField) (time.Time, bool) {
	seconds, ok := pbUint(fields, 1)
	if !ok {
		return time.Time{}, false
	}
	nanos, _ := pbUint(fields, 2)
	if nanos > 999999999 || seconds > 253402300799 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), int64(nanos)).UTC(), true //nolint:gosec // 上限已校验
}

// grpcFrame 给一条消息加 gRPC 长度前缀帧头（压缩标志 0 + 大端长度）
func grpcFrame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// grpcReadFrame 从流里读出一个 gRPC 帧。流在帧边界干净结束返回 io.EOF，
// 读到半截返回 io.ErrUnexpectedEOF。
func grpcReadFrame(r io.Reader, max int) ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:1]); err != nil {
		return nil, err
	}
	if head[0] != 0 {
		return nil, errors.New("上游返回了压缩的 gRPC 帧")
	}
	if _, err := io.ReadFull(r, head[1:]); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > uint32(max) {
		return nil, fmt.Errorf("gRPC 帧 %d 字节超过上限 %d", n, max)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}
