package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// hop 逐跳头，不参与透传
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// 会话粘性：客户端带 X-Session-ID 头时，同一会话绑定同一上游，
// 仅当绑定上游失败（fallback）才按规则重新选择并重新绑定。仅存内存，重启即清。
type session struct {
	modelID string
	expires time.Time
}

const sessionTTL = time.Hour

func (s *Server) getSession(key string) (string, bool) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[key]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, key)
		return "", false
	}
	return sess.modelID, true
}

func (s *Server) setSession(key, modelID string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	if len(s.sessions) > 1024 { // 顺手清理过期会话
		now := time.Now()
		for k, v := range s.sessions {
			if now.After(v.expires) {
				delete(s.sessions, k)
			}
		}
	}
	s.sessions[key] = session{modelID: modelID, expires: time.Now().Add(sessionTTL)}
}

func (s *Server) clearSession(key string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	delete(s.sessions, key)
}

// handleProxy 生成 /chat/completions 与 /responses 共用的透传 handler。
// 请求体只读取 model 字段用于路由，其余字节原样转发；响应字节级回传。
func (s *Server) handleProxy(suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID, ok := s.checkAPIToken(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
			writeErr(w, http.StatusBadRequest, `请求体必须是 JSON 且包含 "model" 字段`)
			return
		}

		// auto 虚拟模型：对外统一声明一个名字，按全局优先级自动挑选实际模型
		autoName := s.store.AutoModelName()
		isAuto := req.Model == autoName

		// 会话绑定的候选排最前（不套用静音/上限规则），规则候选作为 fallback 池
		sessKey := ""
		if sid := r.Header.Get("X-Session-ID"); sid != "" {
			sessKey = sid + "\x00" + req.Model
		}
		var cands []Candidate
		if sessKey != "" {
			if modelID, ok := s.getSession(sessKey); ok {
				if c, ok := s.store.CandidateByID(modelID); ok {
					cands = append(cands, c)
				}
			}
		}
		rule := s.store.Candidates(req.Model, time.Now())
		if isAuto {
			rule = s.store.AllCandidates(time.Now())
		}
		for _, c := range rule {
			if len(cands) == 0 || cands[0].Model.ID != c.Model.ID {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			if isAuto {
				writeErr(w, http.StatusNotFound, "当前没有可用模型（全部未配置、处于静音时段或已达每日上限）")
			} else {
				writeErr(w, http.StatusNotFound,
					fmt.Sprintf("模型 %q 当前没有可用上游（未配置、处于静音时段或已达每日上限）", req.Model))
			}
			return
		}

		lastErr := fmt.Sprintf("模型 %q 无候选", req.Model)
		for _, c := range cands {
			sendBody := body
			if isAuto {
				sendBody = withModel(body, c.Model.Name) // 仅替换顶层 model 字段的值
			}
			up, err := s.upstreamRequest(r, c.Provider, suffix, sendBody)
			if err != nil {
				lastErr = err.Error()
				continue
			}
			resp, err := s.client.Do(up)
			if err != nil {
				lastErr = fmt.Sprintf("%s: %v", c.Provider.Name, err)
				continue
			}
			// 任何 4xx/5xx 都换下一个候选（换一家可能就好了），失败原因带上上游错误信息
			if resp.StatusCode >= 400 {
				eb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
				resp.Body.Close()
				lastErr = fmt.Sprintf("%s: HTTP %d: %s", c.Provider.Name, resp.StatusCode, errSnippet(eb))
				continue
			}
			if sessKey != "" {
				s.setSession(sessKey, c.Model.ID) // 成功即绑定/续期
			}
			s.relay(w, resp, c.Model, keyID)
			return
		}
		if sessKey != "" {
			s.clearSession(sessKey) // 全部失败，解除绑定，下次会话从头按规则选
		}
		writeErr(w, http.StatusBadGateway, "所有上游均失败，最后一个错误: "+lastErr)
	}
}

// withModel 只替换请求体顶层 model 字段的值，其余字段以 RawMessage 原样保留（auto 虚拟模型用）
func withModel(body []byte, name string) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	top["model"], _ = json.Marshal(name)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// upstreamRequest 构造发往上游的请求：按协议选择基址，复用客户端的头（去掉鉴权/逐跳头），换成 provider 的密钥
func (s *Server) upstreamRequest(r *http.Request, p Provider, suffix string, body []byte) (*http.Request, error) {
	base := p.BaseURL
	if suffix == "/responses" && p.ResponsesURL != "" {
		base = p.ResponsesURL
	}
	url := strings.TrimRight(base, "/") + suffix
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vv := range r.Header {
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
	case s.apiToken != "":
		up.Header.Del("Authorization") // 不把本网关的令牌泄漏给上游
	}
	return up, nil
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

// relay 把上游响应原样写回客户端；非流式响应顺带读取 usage 记账
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, m Model, keyID string) {
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		if skipRespHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == 204 || resp.StatusCode == 304 {
		s.store.AddUsage(&m, keyID, 0, 0)
		return
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		io.Copy(flushWriter{w}, resp.Body) // 流式：边收边发，不解析
		s.store.AddUsage(&m, keyID, 0, 0)
		return
	}
	buf, _ := io.ReadAll(resp.Body)
	var u struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"` // chat completions
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"` // responses 协议
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(buf, &u)
	pt, ct := int64(0), int64(0)
	if u.Usage != nil {
		if pt, ct = u.Usage.PromptTokens, u.Usage.CompletionTokens; pt == 0 && ct == 0 {
			pt, ct = u.Usage.InputTokens, u.Usage.OutputTokens
		}
	}
	w.Write(buf)
	s.store.AddUsage(&m, keyID, pt, ct)
}

func skipRespHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	for _, h := range hopHeaders {
		if k == h {
			return true
		}
	}
	return false
}

// handleModels 聚合所有对外模型名，兼容 OpenAI 的 /v1/models
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkAPIToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	names := s.store.ModelNames()
	data := []map[string]any{{"id": s.store.AutoModelName(), "object": "model", "owned_by": "llmrouter"}}
	for _, n := range names {
		data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "llmrouter"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
