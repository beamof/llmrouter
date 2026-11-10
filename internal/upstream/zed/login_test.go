package zed

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// zedFakeCloud 伪造 cloud.zed.dev：llm_tokens / models / users/me / completions
type zedFakeCloud struct {
	base       string
	jwt        string // 换发的 JWT（zedTestJWT 生成）
	models     string
	account    string
	native     atomic.Value // 收到的原生凭据（llm_tokens 校验后的留档）
	mintMu     sync.Mutex
	mintBodies []string         // 每次 llm_tokens 收到的请求 body（锁定空 body 契约）
	handler    http.HandlerFunc // completions 覆盖（可选）
}

func zedTestJWT(t *testing.T, ttl time.Duration) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(ttl).Unix())))
	return header + "." + payload + ".sig"
}

// zedStartFakeCloud 起一个伪造云端，返回注入了 zedBase 的 Server 与 app

// zedInjectLogin 把一个可解密的登录会话塞进 Server（等价于 handleZedLoginStart 的内部装配），
// 返回该会话合法的回调 URL（用会话公钥加密原生凭据）。

func TestZedLoginBadCallback(t *testing.T) {
	login, err := newZedLogin()
	if err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"a":1}`)
	ct, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, &login.privateKey.PublicKey, native, nil)
	good := "http://127.0.0.1:99/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(ct)
	if _, _, err := login.decryptCallback(good); err != nil {
		t.Fatalf("合法回调应通过: %v", err)
	}
	// 路径不限：Zed 页面可能重定向到任意路径，只认查询串（对齐官方客户端与 magpie）
	for _, ok := range []string{
		"http://127.0.0.1:99/callback?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(ct),
		"http://127.0.0.1:99?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(ct), // 粘贴丢尾部斜杠
		"http://localhost:99/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(ct),
	} {
		if _, _, err := login.decryptCallback(ok); err != nil {
			t.Fatalf("回调 %q 应通过: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://127.0.0.1:99/?user_id=u&access_token=x",                                // 非 http
		"http://example.com/?user_id=u&access_token=x",                                  // 非回环
		"http://127.0.0.1:99/?user_id=u",                                                // 缺 access_token
		"http://127.0.0.1:99/?access_token=" + base64.RawURLEncoding.EncodeToString(ct), // 缺 user_id
	} {
		if _, _, err := login.decryptCallback(bad); err == nil {
			t.Fatalf("回调 %q 应被拒绝", bad)
		}
	}
	// 他人公钥加密的密文两种填充都解不开（复制了别的会话的回调）
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	wrong, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, &other.PublicKey, native, nil)
	if _, _, err := login.decryptCallback("http://127.0.0.1:1/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(wrong)); err == nil {
		t.Fatal("他人公钥加密的密文应解密失败")
	}
	wrongV15, _ := rsa.EncryptPKCS1v15(rand.Reader, &other.PublicKey, native)
	if _, _, err := login.decryptCallback("http://127.0.0.1:1/?user_id=u&access_token=" + base64.RawURLEncoding.EncodeToString(wrongV15)); err == nil {
		t.Fatal("他人公钥加密的 v1.5 密文应解密失败")
	}
}

// TestZedLoginV0Callback 服务端 V0 格式（PKCS#1 v1.5）密文也要能解开：
// Zed 官方客户端 decrypt_string 先试 OAEP 再回退 v1.5，两种格式服务端并存。

func TestZedLoginV0Callback(t *testing.T) {
	login, err := newZedLogin()
	if err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"access_token":"native-secret"}`)
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, &login.privateKey.PublicKey, native)
	if err != nil {
		t.Fatal(err)
	}
	uid, got, err := login.decryptCallback("http://127.0.0.1:99/?user_id=user-1&access_token=" + base64.RawURLEncoding.EncodeToString(ct))
	if err != nil {
		t.Fatalf("V0 (PKCS#1 v1.5) 密文应能解开: %v", err)
	}
	if uid != "user-1" || string(got) != string(native) {
		t.Fatalf("解密结果不对: %q %s", uid, got)
	}
}

// TestZedAuthorizationURL 授权地址参数：native_app_port / native_app_public_key 恒有，
// system_id 非空时携带（对齐 Zed 原生客户端 NativeAppSignInQueryParams）。

func TestZedAuthorizationURL(t *testing.T) {
	login, err := newZedLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(login.authorizationURL(1234, ""))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "zed.dev" || u.Path != "/native_app_signin" {
		t.Fatalf("授权地址形态不对: %s", u)
	}
	if got := u.Query().Get("native_app_port"); got != "1234" {
		t.Fatalf("native_app_port = %q", got)
	}
	if key := u.Query().Get("native_app_public_key"); key == "" {
		t.Fatal("缺少 native_app_public_key")
	} else if der, err := base64.RawURLEncoding.DecodeString(key); err != nil || len(der) == 0 {
		t.Fatalf("native_app_public_key 不是合法 base64url: %v", err)
	}
	if _, ok := u.Query()["system_id"]; ok {
		t.Fatal("无 system_id 时不应携带该参数")
	}
	u, err = url.Parse(login.authorizationURL(4321, "0ddc2e22-b830-4db5-a0e4-1f9a2f57f6eb"))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("system_id"); got != "0ddc2e22-b830-4db5-a0e4-1f9a2f57f6eb" {
		t.Fatalf("system_id = %q", got)
	}
}

func TestZedPickOrganization(t *testing.T) {
	tr := true
	fl := false
	orgs := []zedOrg{{ID: "personal", IsPersonal: true}, {ID: "biz"}}
	// 默认组织有付费套餐 → 直接用它
	if got := zedPickOrganization("personal", orgs, map[string]string{"personal": "zed_pro"}, nil); got != "personal" {
		t.Fatalf("默认组织可用应选默认: %q", got)
	}
	// 默认组织免费、另一个有付费 → 选有套餐的
	if got := zedPickOrganization("personal", orgs, map[string]string{"personal": "zed_free", "biz": "zed_pro"}, nil); got != "biz" {
		t.Fatalf("应选有套餐的组织: %q", got)
	}
	// 默认组织有套餐但被组织策略关闭模型 → 无可用组织，回落默认组织（与 magpie 一致）
	if got := zedPickOrganization("personal", orgs, map[string]string{"personal": "zed_pro"},
		map[string]zedOrgConfig{"personal": {IsZedModelProviderEnabled: &fl}}); got != "personal" {
		t.Fatalf("唯一组织被策略关闭时应回落默认组织: %q", got)
	}
	// 多个可用组织：个人优先于顺序上的首个
	mixed := []zedOrg{{ID: "biz1"}, {ID: "personal", IsPersonal: true}}
	if got := zedPickOrganization("", mixed, map[string]string{"biz1": "zed_pro", "personal": "zed_pro"},
		map[string]zedOrgConfig{"biz1": {IsZedModelProviderEnabled: &tr}}); got != "personal" {
		t.Fatalf("可用组织里个人优先: %q", got)
	}
	// 全都不可用 → 回落默认组织，再退首个组织
	if got := zedPickOrganization("biz", orgs, map[string]string{"personal": "", "biz": "zed_free"}, nil); got != "biz" {
		t.Fatalf("全不可用应回落默认组织: %q", got)
	}
	if got := zedPickOrganization("", orgs, nil, nil); got != "personal" {
		t.Fatalf("无默认时退首个组织: %q", got)
	}
	// 没有任何组织 → 空字符串（上游对空组织宽容）
	if got := zedPickOrganization("", nil, nil, nil); got != "" {
		t.Fatalf("无组织应为空: %q", got)
	}
}

func TestZedJWTExpiryParse(t *testing.T) {
	tok := zedTestJWT(t, 90*time.Minute)
	if _, err := zedJWTExpiry(tok); err != nil {
		t.Fatalf("合法 JWT: %v", err)
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d"} {
		if _, err := zedJWTExpiry(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
	// payload 非 JSON 的 JWT
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("!!!!")) + ".x"
	_ = notJSON
}

func TestZedErrorClassification(t *testing.T) {
	if !zedCredentialRejected(401, nil) {
		t.Fatal("401 应判凭据拒绝")
	}
	if !zedCredentialRejected(403, []byte(`{"error":"unauthorized"`)) {
		t.Fatal("不含 plan 的 403 应判凭据拒绝")
	}
	if zedCredentialRejected(403, []byte(`{"error":"your plan does not include this model"}`)) {
		t.Fatal("含 plan 的 403 不应判凭据拒绝")
	}
	if !zedModelPlanRejected(403, []byte(`Upgrade your PLAN`)) {
		t.Fatal("大小写不敏感的 plan 检测")
	}
	if zedModelPlanRejected(403, nil) || zedModelPlanRejected(401, []byte("plan")) {
		t.Fatal("非 403 不算套餐拒绝")
	}
}

func TestZedTokenExpiring(t *testing.T) {
	soon := time.Now().Add(30 * time.Second)
	c := &ZedCred{AccessToken: "t", ExpiresAt: &soon}
	if !tokenExpiring(c) {
		t.Fatal("30 秒后过期应视为临期（窗口 2 分钟）")
	}
	later := time.Now().Add(time.Hour)
	c.ExpiresAt = &later
	if tokenExpiring(c) {
		t.Fatal("1 小时后过期不应临期")
	}
	if !tokenExpiring(&ZedCred{}) {
		t.Fatal("无 token 应视为临期")
	}
}

// TestZedUpstreamModels Zed provider 的「上游模型」下拉必须走 zedCatalog
// （cloud.zed.dev/models + JWT + 原生客户端头），不能拿 BaseURL 拼 /models
// ——那是 completions 端点，拼出来的地址必 404/405。
