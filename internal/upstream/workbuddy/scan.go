package workbuddy

// ─────────────── WorkBuddy 本机凭证扫描与导入 ───────────────
//
// 官方客户端（WorkBuddy 桌面端 / CodeBuddy 插件，内核相同）在登录时把会话快照写进
// CodeBuddyExtension 的 Data/Public/auth 目录，每个账号一份 .info JSON（{account, auth}
// 形状，含 uid/accessToken/refreshToken 等）。v5.6.2 起敏感字段存为 AES-256-GCM 加密
// 信封，密钥是客户端构建期静态串、不在磁盘上——需用户另行提供才能解开；密钥字段值
// 形如 {"$wbEncrypted":1,"envelope":"<base64>"}。
//
// 扫描是一次性引导：导入后 llmrouter 持有 refreshToken 自续期，与客户端各自独立。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// wbScanHit 扫描到的一条账号凭证
type wbScanHit struct {
	UID       string         `json:"uid"`
	Nickname  string         `json:"nickname,omitempty"`
	Source    string         `json:"source"`         // 来源文件路径
	App       string         `json:"app,omitempty"`  // 来源客户端（桌面端/插件）
	Encrypted bool           `json:"encrypted"`      // 凭证为加密信封且未能解开
	Note      string         `json:"note,omitempty"` // 给后台展示的说明
	Cred      *WorkBuddyCred `json:"cred,omitempty"` // 成功解开时的完整凭证（导入时回传）
	modTime   time.Time      // 同一 uid 多份快照时取最新
}

// wbScan 扫描本机候选位置，按 uid 去重（保留修改时间最新的一份）
func wbScan(secret string) []*wbScanHit {
	hits := map[string]*wbScanHit{}
	add := func(path string) {
		hit := wbScanFile(path, secret)
		if hit == nil {
			return
		}
		if old, ok := hits[hit.UID]; !ok || hit.modTime.After(old.modTime) {
			hits[hit.UID] = hit
		}
	}
	// 客户端登录态快照目录：每账号一份 *.info
	if dir := wbAuthDir(); dir != "" {
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".info") {
					add(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	// CLI 点目录兜底：递归找 .json/.info（深度 ≤3，小文件）
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{".workbuddy", ".workbuddy-ai", ".codebuddy"} {
			root := filepath.Join(home, d)
			filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
				if err != nil || path == root {
					return nil
				}
				rel := strings.TrimPrefix(path, root+string(filepath.Separator))
				if strings.Count(rel, string(filepath.Separator)) >= 3 {
					if de.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if !de.IsDir() && (strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".info")) {
					add(path)
				}
				return nil
			})
		}
	}
	out := make([]*wbScanHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// wbAuthDir 客户端登录态快照目录（按平台）
func wbAuthDir() string {
	switch runtime.GOOS {
	case "windows":
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "CodeBuddyExtension", "Data", "Public", "auth")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth")
		}
	default:
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Data", "Public", "auth")
		}
	}
	return ""
}

// wbAppOf 从快照文件名推断来源客户端
func wbAppOf(name string) string {
	switch {
	case strings.HasPrefix(name, "workbuddy-desktop"):
		return "WorkBuddy 桌面端"
	case strings.HasPrefix(name, "Tencent-Cloud.coding-copilot"):
		return "CodeBuddy 插件"
	}
	return ""
}

// wbScanFile 解析单个候选文件；不是凭证形态或超过 1MB 时返回 nil
func wbScanFile(path, secret string) *wbScanHit {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() == 0 || st.Size() > 1<<20 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	hit := wbParseInfoBytes(data, secret)
	if hit == nil {
		return nil
	}
	hit.Source = path
	hit.modTime = st.ModTime()
	if hit.App == "" {
		hit.App = wbAppOf(filepath.Base(path))
	}
	return hit
}

// wbParseInfoBytes 解析 .info 形态的会话 JSON：{account:{…}, auth:{…}}，兼容扁平形态
// （顶层直接放 uid/accessToken）。凭证字段可能是明文，也可能是加密信封——
// 解不开时返回 Encrypted=true 的占位命中（uid 已知、无 cred），供后台提示补密钥。
func wbParseInfoBytes(data []byte, secret string) *wbScanHit {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	account, _ := m["account"].(map[string]any)
	auth, _ := m["auth"].(map[string]any)
	pick := func(key string) any {
		for _, src := range []map[string]any{auth, account, m} {
			if src == nil {
				continue
			}
			if v, ok := src[key]; ok && v != nil {
				return v
			}
		}
		return nil
	}
	str := func(key string) string {
		s, _ := pick(key).(string)
		return strings.TrimSpace(s)
	}
	uid := str("uid")
	if uid == "" {
		return nil
	}
	hit := &wbScanHit{UID: uid}
	accessToken, _ := wbOpen(pick("accessToken"), secret)
	if accessToken == "" {
		if _, isEnv := wbEnvelopeOf(pick("accessToken")); isEnv {
			hit.Encrypted = true
			hit.Note = "凭证为加密信封（客户端 v5.6.2+），需提供 atRestSecretKey 才能导入"
			return hit // 加密占位：uid 已知，提示补密钥
		}
		return nil // 连 accessToken 字段都没有，不是凭证形态
	}
	refreshToken, _ := wbOpen(pick("refreshToken"), secret)
	nickname, _ := wbOpen(pick("nickname"), secret)
	cred := &WorkBuddyCred{
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Domain:       str("domain"),
		TokenType:    str("tokenType"),
		EnterpriseID: str("enterpriseId"),
		Department:   str("departmentFullName"),
	}
	if f, ok := pick("expiresAt").(float64); ok && f > 0 {
		cred.ExpiresAt = wbTimePtr(time.UnixMilli(int64(f)))
	}
	if f, ok := pick("refreshExpiresAt").(float64); ok && f > 0 {
		cred.RefreshExpiresAt = wbTimePtr(time.UnixMilli(int64(f)))
	}
	hit.Cred = cred
	hit.Nickname = nickname
	if hit.Nickname == "" {
		hit.Nickname = uid
		if len(uid) > 8 {
			hit.Nickname = uid[:8]
		}
	}
	return hit
}

// wbOpen 打开一个凭证字段：明文字符串直接返回；加密信封用密钥解开。
// 第二个返回值表示该字段是加密信封。
func wbOpen(v any, secret string) (string, bool) {
	if s, ok := v.(string); ok {
		return s, false
	}
	env, ok := wbEnvelopeOf(v)
	if !ok {
		return "", false
	}
	if secret == "" {
		return "", true
	}
	s, err := wbOpenEnvelope(env, secret)
	if err != nil {
		return "", true
	}
	return s, true
}

// wbEnvelopeOf 取字段值里的信封 base64（形如 {"$wbEncrypted":1,"envelope":"…"}）
func wbEnvelopeOf(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	if b, ok := m["$wbEncrypted"].(float64); !ok || b != 1 {
		return "", false
	}
	env, _ := m["envelope"].(string)
	return env, env != ""
}

// wbOpenEnvelope 解开 $wbEncrypted 信封：base64 内是
// {suite,keyId,nonce,authTag,ciphertext}，AES-256-GCM + WB-AAD 帧。
// key = sha256(密钥串)；keyId = sha256(key) hex 前 16 位，须与信封一致。
func wbOpenEnvelope(envB64, secret string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		return "", fmt.Errorf("信封不是合法 base64: %v", err)
	}
	var env struct {
		Suite      int    `json:"suite"`
		KeyID      string `json:"keyId"`
		Nonce      string `json:"nonce"`
		AuthTag    string `json:"authTag"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("信封不是合法 JSON: %v", err)
	}
	if env.Suite != 1 {
		return "", fmt.Errorf("未知加密套件 %d", env.Suite)
	}
	key := sha256.Sum256([]byte(secret))
	keyHash := sha256.Sum256(key[:])
	if want := hex.EncodeToString(keyHash[:])[:16]; env.KeyID != want {
		return "", fmt.Errorf("密钥与信封不匹配（keyId=%s），请核对 atRestSecretKey", env.KeyID)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return "", fmt.Errorf("nonce 非法")
	}
	tag, err := base64.StdEncoding.DecodeString(env.AuthTag)
	if err != nil || len(tag) != 16 {
		return "", fmt.Errorf("authTag 非法")
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("ciphertext 非法")
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, append(ct, tag...), wbAAD(env.KeyID, env.Suite))
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥错误或数据损坏）: %v", err)
	}
	return string(pt), nil
}

// wbAAD 构造信封解密的附加认证数据（"WB-AAD" 帧，字符串字段带 4 字节大端长度前缀）
func wbAAD(keyID string, suite int) []byte {
	aad := []byte("WB-AAD\x00")
	aad = append(aad, 0x01)
	aad = wbLenPfx(aad, "WBEV1")
	aad = wbLenPfx(aad, "sym-v1")
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(suite))
	aad = append(aad, b[:]...)
	aad = wbLenPfx(aad, keyID)
	return append(aad, 0x02, 0x00, 0x00)
}

func wbLenPfx(dst []byte, s string) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(s)))
	dst = append(dst, b[:]...)
	return append(dst, s...)
}
