package main

import (
	"encoding/json"
	"net/http"
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
	auth := cred.nativeAuthorization()
	if auth != `user-1 {"a":1,"b":{"c":2}}` {
		t.Fatalf("Authorization = %q，期望紧凑 JSON", auth)
	}
	if strings.ContainsAny(auth, "\r\n\t") {
		t.Fatalf("Authorization 仍含控制字符: %q", auth)
	}
	if got := (*ZedCred)(nil).nativeAuthorization(); got != "" {
		t.Errorf("nil 凭证应返回空串，得到 %q", got)
	}
	// 凭据不是合法 JSON 时也要抹掉控制字符，让请求发出去、由上游报错
	bad := &ZedCred{UserID: "u", NativeCredential: json.RawMessage("{\n bad \n}")}
	if got := bad.nativeAuthorization(); strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("非法凭据仍含控制字符: %q", got)
	}
}

// TestZedMintAfterStoreReload 回归：凭证落盘再读回（带缩进）后仍能重铸 JWT。
// 旧实现下 zedMintLLMToken 会在构造请求时失败，表现为
// 「重铸请求失败: Post .../llm_tokens: net/http: invalid header field value」。
func TestZedMintAfterStoreReload(t *testing.T) {
	fc, s, _ := zedStartFakeCloud(t)
	p := &Provider{
		Kind: "zed",
		Zed: &ZedCred{
			UserID:           "user-1",
			OrganizationID:   "org-1",
			NativeCredential: json.RawMessage(`{"access_token":"native-token","user_id":"user-1"}`),
		},
	}
	s.store.UpsertProvider(p)

	reloaded, err := LoadStore(s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Provider(p.ID)
	if !ok || got.Zed == nil {
		t.Fatal("provider 未随 data.json 落盘")
	}
	// 测试前提：读回的凭据确实被 MarshalIndent 美化过（否则回归无从复现）
	if !strings.Contains(string(got.Zed.NativeCredential), "\n") {
		t.Fatalf("测试前提不成立：落盘后的凭据应带缩进，实际 %q", got.Zed.NativeCredential)
	}
	minted, err := zedMintLLMToken(t.Context(), &http.Client{}, fc.base, got.Zed)
	if err != nil {
		t.Fatalf("重载后重铸失败: %v", err)
	}
	if minted.AccessToken != fc.jwt {
		t.Fatalf("重铸 token = %q，期望 %q", minted.AccessToken, fc.jwt)
	}
	if got, want := fc.nativeValue(), `{"access_token":"native-token","user_id":"user-1"}`; got != want {
		t.Errorf("上游收到的原生凭据 = %q，期望紧凑 JSON %q", got, want)
	}
}

// nativeValue 读取 fake cloud 收到的原生凭据（无请求时返回空串）
func (fc *zedFakeCloud) nativeValue() string {
	if v, ok := fc.native.Load().(string); ok {
		return v
	}
	return ""
}
