package geminiapp

// JSON 小工具：数字按原文字保留（不经过 float64），序列化不转义 HTML 字符，
// 与提示词 / 历史回放里引用的 JSON 文本形态保持稳定。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
)

// decodeJSONNumber 解析一段 JSON，数字解码成 json.Number（保留原文精度）
func decodeJSONNumber(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errTrailingJSON
	}
	return v, nil
}

var errTrailingJSON = errors.New("JSON 之后还有多余内容")

// compactJSON 去掉一段 JSON 文本里的空白（不改键顺序与数字写法）
func compactJSON(raw []byte) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// encodeCompactJSON 紧凑序列化（不转义 < > &）
func encodeCompactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// mustCompactJSON 同 encodeCompactJSON；入参都来自已成功解析的请求体，不会失败
func mustCompactJSON(v any) string {
	b, err := encodeCompactJSON(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// utf16Length 字符串按 UTF-16 码元计的长度（用量估算口径）
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// numberRat 把数字（json.Number 或 float64）转成精确有理数
func numberRat(v any) (*big.Rat, bool) {
	var r big.Rat
	switch x := v.(type) {
	case json.Number:
		if _, ok := r.SetString(x.String()); ok {
			return &r, true
		}
	case float64:
		if rt := new(big.Rat).SetFloat64(x); rt != nil {
			return rt, true
		}
	}
	return nil, false
}

// isIntegerNumber 数字是否为整数
func isIntegerNumber(v any) bool {
	r, ok := numberRat(v)
	return ok && r.IsInt()
}

// jsonEqual 两个 JSON 值是否相等（数字按数值比较，对象不看键顺序）
func jsonEqual(a, b any) bool {
	if ra, ok := numberRat(a); ok {
		rb, ok := numberRat(b)
		return ok && ra.Cmp(rb) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(v, yv) {
				return false
			}
		}
		return true
	}
	return false
}

// canonicalJSON 判等用的规范化文本（对象键排序）
func canonicalJSON(v any) string {
	b, err := encodeCompactJSON(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeArgsObject 把工具实参文本解析成对象；解析结果只用于提示词里的历史回放
func decodeArgsObject(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}
