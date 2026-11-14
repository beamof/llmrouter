package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"llmrouter/internal/domain"
	"llmrouter/internal/store"
	"llmrouter/internal/wire"
)

// hop 逐跳头，不参与透传
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Standard 标准 OpenAI 兼容上游适配器：请求与响应字节级透传。
type Standard struct {
	Store    *store.Store
	Clients  *ClientFactory
	APIToken string // 本网关的入口令牌；非空时剥掉客户端 Authorization，避免泄漏给上游
}

// Kinds 标准上游的 Kind 为空串。
func (s *Standard) Kinds() []string { return []string{""} }

// CanServe 标准上游两种接入协议都原样透传。
func (s *Standard) CanServe(domain.Protocol) bool { return true }

// ValidateProvider 校验基址。
func (s *Standard) ValidateProvider(p *domain.Provider) error {
	if strings.TrimSpace(p.BaseURL) == "" {
		return fmt.Errorf("base_url 不能为空")
	}
	return nil
}

// Attempt 透传一个候选：构造请求 → 上游调用 → 响应回传 + 用量记账。
func (s *Standard) Attempt(a *Attempt) error {
	defer s.Store.Release(a.Candidate.Model.ID)
	up, err := s.buildRequest(a)
	if err != nil {
		return err
	}
	cli, err := s.Clients.For(a.Candidate.Provider, 0)
	if err != nil {
		return err
	}
	resp, err := cli.Do(up)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		// 1MB 足够覆盖正常错误体；直连请求会把状态码与原始 body 透传给客户端。
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return &StatusError{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: eb}
	}
	s.relay(a, resp)
	return nil
}

// buildRequest 构造发往上游的请求：按协议选择基址，复用客户端的头（去掉鉴权/逐跳头），
// 换成 provider 的密钥。
func (s *Standard) buildRequest(a *Attempt) (*http.Request, error) {
	p := a.Candidate.Provider
	suffix := "/chat/completions"
	if a.Protocol == domain.ProtocolResponses {
		suffix = "/responses"
	}
	base := p.BaseURL
	if a.Protocol == domain.ProtocolResponses && p.ResponsesURL != "" {
		base = p.ResponsesURL
	}
	url := strings.TrimRight(base, "/") + suffix
	if a.Request.URL.RawQuery != "" {
		url += "?" + a.Request.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(a.Request.Context(), http.MethodPost, url, bytes.NewReader(a.Body))
	if err != nil {
		return nil, err
	}
	for k, vv := range a.Request.Header {
		if skipReqHeader(k) {
			continue
		}
		for _, v := range vv {
			up.Header.Add(k, v)
		}
	}
	if up.Header.Get("Content-Type") == "" {
		up.Header.Set("Content-Type", "application/json")
	}
	if up.Header.Get("Accept-Encoding") == "" {
		// 不让 transport 透明加 gzip 再解压，保证字节级透传
		up.Header.Set("Accept-Encoding", "identity")
	}
	switch {
	case p.APIKey != "":
		up.Header.Set("Authorization", "Bearer "+p.APIKey)
	case s.APIToken != "":
		up.Header.Del("Authorization") // 不把本网关的令牌泄漏给上游
	}
	return up, nil
}

// relay 把上游响应原样写回客户端，并把本次调用的用量与耗时写入 a.Usage。
func (s *Standard) relay(a *Attempt, resp *http.Response) {
	defer resp.Body.Close()
	w := a.Writer
	for k, vv := range resp.Header {
		if SkipRespHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == 204 || resp.StatusCode == 304 {
		a.Usage.Apply(a.Timing.Sample(domain.Usage{}, false))
		return
	}
	body := &FirstByteReader{R: resp.Body, T: a.Timing}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sn := &wire.Sniffer{W: wire.FlushWriter{W: w}}
		io.Copy(sn, body) // 流式：边收边发，字节不变，尾部嗅探 usage
		a.Usage.Apply(a.Timing.Sample(wire.ExtractStreamUsage(sn.Tail), true))
		return
	}
	buf, _ := io.ReadAll(body)
	var top struct {
		Usage json.RawMessage `json:"usage"`
	}
	json.Unmarshal(buf, &top)
	w.Write(buf)
	a.Usage.Apply(a.Timing.Sample(wire.ParseUsage(top.Usage), false))
}

// Probe 标准上游的连通性探测：GET /models，顺带核对模型名。
func (s *Standard) Probe(ctx context.Context, p domain.Provider, model string) (ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return ProbeResult{Level: "fail", Message: err.Error()}, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	cli, cerr := s.Clients.For(p, 15*time.Second)
	if cerr != nil {
		return ProbeResult{Level: "fail", Message: cerr.Error()}, nil
	}
	resp, err := cli.Do(req)
	if err != nil {
		return ProbeResult{Level: "fail", Message: "连接失败: " + err.Error()}, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res := ProbeResult{Status: resp.StatusCode}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Level = "warn"
		res.Message = fmt.Sprintf("端口通（HTTP %d，鉴权失败，请检查 API Key）: %s", resp.StatusCode, ErrSnippet(b))
	case resp.StatusCode >= 400:
		res.Level = "warn"
		res.Message = fmt.Sprintf("端口通，但 /models 返回 HTTP %d（不影响对话接口）", resp.StatusCode)
	default:
		res.Level = "ok"
		res.Message = "端口通"
		if ids := ExtractModelIDs(b); len(ids) > 0 {
			res.Message = fmt.Sprintf("端口通，模型名在上游列表中（共 %d 个）", len(ids))
			if !contains(ids, model) {
				res.Level = "warn"
				res.Message = fmt.Sprintf("端口通，但上游列表（%d 个）中没有 %q，请核对模型名", len(ids), model)
			}
		}
	}
	return res, nil
}

func skipReqHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	if k == "Authorization" || k == "Host" || k == "Content-Length" {
		return true
	}
	for _, h := range hopHeaders {
		if k == h {
			return true
		}
	}
	return false
}

// SkipRespHeader 响应逐跳头判定（适配器共用）。
func SkipRespHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	for _, h := range hopHeaders {
		if k == h {
			return true
		}
	}
	return false
}

// ListModels 标准上游的模型目录：GET {base}/models。
func (s *Standard) ListModels(ctx context.Context, p domain.Provider) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	cli, err := s.Clients.For(p, 15*time.Second)
	if err != nil {
		return nil, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, ErrSnippet(b))
	}
	return ExtractModelIDs(b), nil
}
