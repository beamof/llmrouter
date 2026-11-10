//go:build legacy

package zed

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"llmrouter/internal/store"
)

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
	s.rt.Store.UpsertProvider(p)

	reloaded, err := store.LoadStore(s.rt.Store.Path())
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
