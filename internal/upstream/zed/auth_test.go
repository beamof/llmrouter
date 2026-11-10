package zed

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestZedNativeAuthorizationCompact 回归：data.json 用 MarshalIndent 落盘时，
// json.RawMessage 里的原生凭据会被重新缩进（带换行与空格）。Authorization 头
// 不允许控制字符，原样拼进去会让 net/http 直接报
// `invalid header field value for "Authorization"`，请求发不出去。
func TestZedNativeAuthorizationCompact(t *testing.T) {
	cred := &ZedCred{
		UserID:           "user-1",
		NativeCredential: json.RawMessage("{\n  \"a\": 1,\n  \"b\": {\n    \"c\": 2\n  }\n}"),
	}
	auth := nativeAuthorization(cred)
	if auth != `user-1 {"a":1,"b":{"c":2}}` {
		t.Fatalf("Authorization = %q，期望紧凑 JSON", auth)
	}
	if strings.ContainsAny(auth, "\r\n\t") {
		t.Fatalf("Authorization 仍含控制字符: %q", auth)
	}
	if got := nativeAuthorization(nil); got != "" {
		t.Errorf("nil 凭证应返回空串，得到 %q", got)
	}
	// 凭据不是合法 JSON 时也要抹掉控制字符，让请求发出去、由上游报错
	bad := &ZedCred{UserID: "u", NativeCredential: json.RawMessage("{\n bad \n}")}
	if got := nativeAuthorization(bad); strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("非法凭据仍含控制字符: %q", got)
	}
}
