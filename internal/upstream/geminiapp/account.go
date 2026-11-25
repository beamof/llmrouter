package geminiapp

// 账号维护：订阅用量快照（GetUsageInfo）、主服务条款接受、模型目录同步与
// 凭证粘贴导入的解析。

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"llmrouter/internal/domain"
)

// utcNow 快照时间统一存 UTC
func utcNow() time.Time { return time.Now().UTC() }

// tierNames GetUsageInfo.user_tier（原生 UserSubscriptionTier 枚举）→ 档位代码
var tierNames = map[uint64]string{1: "free", 2: "pro", 3: "ultra", 4: "plus", 5: "neon"}

// fetchUsage 只读查询账号的订阅档位与各用量窗口，不产生对话、不改配额
func (a *Adapter) fetchUsage(ctx context.Context, cli *http.Client, token string) (*domain.GeminiAppUsage, error) {
	msg, err := a.unary(ctx, cli, token, usageInfoPath, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return decodeUsageInfo(msg)
}

// decodeUsageInfo 解析 GetUsageInfo 响应：字段 2 是档位，字段 3 是用量分组，
// 组内字段 2 是窗口行（已用百分比 + 窗口名 + 窗口滚动时刻）。窗口滚动不保证
// 用量归零。
func decodeUsageInfo(msg []byte) (*domain.GeminiAppUsage, error) {
	root, err := pbParse(msg)
	if err != nil {
		return nil, errInvalidResponse
	}
	u := &domain.GeminiAppUsage{CheckedAt: utcNow()}
	if tier, ok := pbUint(root, 2); ok {
		u.Tier = tierNames[tier]
	}
	for _, group := range root {
		if group.Number != 3 || group.Wire != 2 {
			continue
		}
		gf, err := pbParse(group.Payload)
		if err != nil {
			continue
		}
		for _, row := range gf {
			if row.Number != 2 || row.Wire != 2 {
				continue
			}
			rf, err := pbParse(row.Payload)
			if err != nil {
				continue
			}
			item := domain.GeminiAppUsageRow{}
			item.Label, _ = pbText(rf, 3)
			pct, hasPct := pbFloat(rf, 2)
			if hasPct && (pct < 0 || pct > 1) {
				continue
			}
			if hasPct {
				item.Percent = math.Round(float64(pct)*10000) / 100 // 百分比，保留两位小数
			}
			if tf, ok := pbGet(rf, 1); ok && tf.Wire == 2 {
				if ts, err := pbParse(tf.Payload); err == nil {
					if at, ok := pbTimestamp(ts); ok {
						item.RefreshAt = &at
					}
				}
			}
			if item.Label != "" || hasPct {
				u.Rows = append(u.Rows, item)
			}
		}
	}
	return u, nil
}

// saveUsage 回写用量快照。查询失败时保留旧快照，只记下失败原因。
// 只改 Usage 字段：并发续期写入的新 token 不会被整份替换覆盖。
func (a *Adapter) saveUsage(providerID string, usage *domain.GeminiAppUsage, queryErr error) {
	p, ok := a.rt.Store.Provider(providerID)
	if !ok || p.GeminiApp == nil {
		return
	}
	if queryErr != nil {
		if old := p.GeminiApp.Usage; old != nil {
			snapshot := *old
			snapshot.Error = queryErr.Error()
			usage = &snapshot
		} else {
			usage = &domain.GeminiAppUsage{Error: queryErr.Error(), CheckedAt: utcNow()}
		}
	}
	a.rt.Store.UpdateGeminiAppUsage(providerID, usage)
}

// acceptTerms 显式接受 Gemini 主服务条款：先查当前版本号，再按该版本提交接受。
// 不借用设备 ID 或应用版本，也不硬编码条款版本；查不到版本号就停下。
func (a *Adapter) acceptTerms(ctx context.Context, cli *http.Client, token string) (int64, error) {
	msg, err := a.unary(ctx, cli, token, termsVersionPath,
		concat(pbString(1, "en"), pbBytes(2, termsRequestMetadata("en"))), 10*time.Second)
	if err != nil {
		return 0, err
	}
	fields, perr := pbParse(msg)
	version, ok := pbUint(fields, 2)
	if perr != nil || !ok || version == 0 {
		return 0, errors.New("条款版本查询没有返回可用的版本号")
	}
	now := time.Now()
	body := concat(
		pbString(4, gatewayUserAgent),
		pbVarint(5, nativeClientType),
		pbVarint(6, version), // 主服务条款的数字版本
		pbBytes(7, termsRequestMetadata("en")),
		pbBytes(10, concat(pbVarint(1, uint64(now.Unix())), pbVarint(2, uint64(now.Nanosecond())))), //nolint:gosec // 时间戳非负
	)
	resp, err := a.unary(ctx, cli, token, termsAcceptPath, body, 10*time.Second)
	if err != nil {
		return 0, err
	}
	if accepted, perr := pbParse(resp); perr != nil {
		return 0, errInvalidResponse
	} else if _, ok := pbGet(accepted, 1); !ok {
		return 0, errInvalidResponse
	}
	return int64(version), nil
}

// syncModels 把内置模型目录同步成该 Provider 的模型条目（只增不删，不覆盖用户
// 调整过的优先级 / 上限 / 暴露开关），返回目录里的模型数
func (a *Adapter) syncModels(providerID string) int {
	n := 0
	for _, name := range nativeModels {
		if !a.rt.Store.HasGeminiAppModel(providerID, name) {
			a.rt.Store.UpsertModel(&Model{ProviderID: providerID, Name: name, Expose: true,
				ContextLength: modelSpecs[name].ContextLength})
		}
		n++
	}
	return n
}

// persistAccount 创建或原地更新 Gemini App Provider，并同步模型条目
func (a *Adapter) persistAccount(cred *AppCred, name, proxy string) (string, int) {
	p := &Provider{Kind: Kind, BaseURL: a.robinOrigin() + processQueryPath, Proxy: proxy, GeminiApp: cred}
	if old, ok := a.rt.Store.ProviderByGeminiAppSubject(cred.Subject); ok {
		p.ID, p.Name, p.Proxy = old.ID, old.Name, old.Proxy // 原地更新：保留 ID / 名称 / 代理
	}
	if p.Name == "" {
		p.Name = strings.TrimSpace(name)
	}
	if p.Name == "" {
		label := cred.Email
		if label == "" {
			if label = cred.Subject; len(label) > 8 {
				label = label[:8]
			}
		}
		p.Name = "Gemini · " + label
	}
	a.rt.Store.UpsertProvider(p)
	return p.ID, a.syncModels(p.ID)
}

// parseCredential 解析粘贴导入的凭证 JSON。兼容三种形态：
//  1. 通用 Google OAuth 凭证（refresh_token / client_id / client_secret）；
//  2. gemini-app2api 状态文件里的账号记录（refreshToken / googleSubject / label）；
//  3. 本网关 data.json 里 provider 的 geminiapp 字段（expires_at 为绝对时刻），
//     或包着它的整个 provider 对象。
//
// refresh_token 缺一不可（无法续期的凭证没有意义）。
func parseCredential(raw string) (*AppCred, error) {
	trimmed := strings.TrimSpace(raw)
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil || top == nil {
		return nil, errors.New("凭证 JSON 无法解析（需要对象形态）")
	}
	if inner, ok := top["geminiapp"]; ok {
		trimmed = string(inner)
	}
	var v struct {
		Subject       string     `json:"subject"`
		GoogleSubject string     `json:"googleSubject"`
		Email         string     `json:"email"`
		Label         string     `json:"label"`
		ClientID      string     `json:"client_id"`
		ClientSecret  string     `json:"client_secret"`
		AccessToken   string     `json:"access_token"`
		RefreshToken  string     `json:"refresh_token"`
		RefreshTokenC string     `json:"refreshToken"`
		ExpiresAt     *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return nil, errors.New("凭证 JSON 无法解析（需要对象形态）")
	}
	cred := &AppCred{
		Subject:      strings.TrimSpace(firstNonEmpty(v.Subject, v.GoogleSubject)),
		Email:        strings.TrimSpace(v.Email),
		ClientID:     strings.TrimSpace(v.ClientID),
		ClientSecret: strings.TrimSpace(v.ClientSecret),
		AccessToken:  strings.TrimSpace(v.AccessToken),
		RefreshToken: strings.TrimSpace(firstNonEmpty(v.RefreshToken, v.RefreshTokenC)),
		ExpiresAt:    v.ExpiresAt,
	}
	if cred.Email == "" && strings.Contains(v.Label, "@") { // 账号记录的 label 通常就是邮箱
		cred.Email = strings.TrimSpace(v.Label)
	}
	if cred.RefreshToken == "" {
		return nil, errors.New("凭证缺少 refresh_token 字段")
	}
	return cred, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
