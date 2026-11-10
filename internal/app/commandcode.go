package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/upstream"
	"llmrouter/internal/upstream/commandcode"
)

const (
	commandCodeKind       = commandcode.Kind
	commandCodeMaxOutput  = commandcode.MaxOutput
	commandCodeDefaultMax = commandcode.DefaultMax
	commandCodeCLIVersion = commandcode.CLIVersion
)

// ccOf 取 Command Code 凭证（Kind 不是 commandcode 时返回 nil）
func ccOf(p Provider) *CommandCodeCred { return commandcode.Cred(p) }

// ccAdapter 构造 Command Code 适配器（装配轻量，直接按需创建）。
func (s *Server) ccAdapter() *commandcode.Adapter {
	return commandcode.New(s.upstreamRuntime(), s.ccBase)
}

// ccAPI 数据面基址（测试可注入 ccBase）
func (s *Server) ccAPI() string { return s.ccAdapter().API() }

// ccBuildBody 兼容旧测试入口。
func ccBuildBody(raw []byte, model string) ([]byte, string, error) {
	return commandcode.BuildBody(raw, model)
}

// handleCommandCodeImport 从本机 CLI 登录态或粘贴的 API Key 导入账号。
func (s *Server) handleCommandCodeImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProviderID string `json:"provider_id"`
		Name       string `json:"name"`
		Proxy      string `json:"proxy"`
		ZDR        bool   `json:"zdr"`
		APIKey     string `json:"api_key"`
	}
	if !decode(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	fromCLI := key == ""
	if fromCLI {
		af, err := commandcode.ReadCLIAuth()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		key = af.APIKey
	}
	proxy, err := upstream.NormalizeProxy(req.Proxy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "proxy 非法: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := s.ccAdapter().PersistProvider(ctx, req.ProviderID, key, req.Name, proxy, req.ZDR, fromCLI)
	if err != nil {
		// Provider 已落盘，模型同步失败只提示，不回滚账号。
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": res.ProviderID, "models": res.Models, "plan": res.Plan, "warning": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": res.ProviderID, "models": res.Models, "plan": res.Plan})
}

// handleCommandCodeRefresh 重新读取套餐并同步模型；CLI 导入的账号会重新读本机
// auth.json，`commandcode login` 换号后自动跟上。
func (s *Server) handleCommandCodeRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	plan, n, err := s.ccAdapter().Refresh(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": plan, "models": n})
}

// ccAttempt 兼容旧候选循环的入口。
func (s *Server) ccAttempt(w http.ResponseWriter, r *http.Request, c Candidate, sendBody []byte, keyID string, responsesMode bool, tm *upstream.Timing, sm *usageSample) error {
	proto := domain.ProtocolChat
	if responsesMode {
		proto = domain.ProtocolResponses
	}
	return s.ccAdapter().Attempt(&upstream.Attempt{
		Writer: w, Request: r, Candidate: c, Body: sendBody,
		KeyID: keyID, Protocol: proto, Timing: tm, Usage: sm,
	})
}

// ccModelTest 用模型目录做连通性探测（不消耗 token）。
func (s *Server) ccModelTest(w http.ResponseWriter, m Model, p Provider) {
	res, _ := s.ccAdapter().Probe(context.Background(), p, m.Name)
	writeJSON(w, http.StatusOK, res)
}

// ccCatalog 兼容旧调用点。
func (s *Server) ccCatalog(ctx context.Context, p Provider) ([]string, error) {
	return s.ccAdapter().Catalog(ctx, p)
}
