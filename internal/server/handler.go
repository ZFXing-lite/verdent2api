// Package server 暴露 OpenAI 兼容接口，把请求路由到账号池选出的 Verdent 账号，
// 走桌面端加密链路（llm-proxy.verdent.ai/llm/stream），命中 Free mode 限免模型。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"verdent2api/internal/pool"
	"verdent2api/internal/verdent"
)

// Server HTTP 网关。
type Server struct {
	Pool     *pool.Pool
	Store    *verdent.Store
	Client   *verdent.Client
	Catalog  *verdent.Catalog
	APIKey   string // 网关侧鉴权 key；空=不鉴权
	FreeOnly bool
	Version  string
	// HTTP 供 token refresh 使用。
	HTTP *http.Client
}

// Register 注册路由。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.auth(s.chatCompletions))
	mux.HandleFunc("/v1/models", s.auth(s.models))
	mux.HandleFunc("/v1/models/", s.auth(s.models))
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/status", s.auth(s.status))
	mux.HandleFunc("/", s.notFound)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.APIKey != "" && bearerToken(r) != s.APIKey {
			s.writeErr(w, http.StatusUnauthorized, "invalid_api_key",
				"Incorrect API key provided.")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return h
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.writeErr(w, http.StatusNotFound, "not_found", "Unknown endpoint: "+r.URL.Path)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	accts := s.Store.All()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"ts":       time.Now().Unix(),
		"accounts": len(accts),
	})
}

// models 输出模型目录（含 is_free 标记）。
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	models := s.Catalog.Models()
	if s.FreeOnly {
		models = s.Catalog.FreeModels()
	}
	out := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		out = append(out, m.OpenAIObject())
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": out})
}

// chatCompletions 主链路：选号 → 取/刷 token → 构造加密 body → 流式翻译。
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "read body failed")
		return
	}
	var req chatReq
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Model == "" {
		req.Model = "deepseek-v4.1-flash-free"
	}
	if len(req.Messages) == 0 {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "messages required")
		return
	}

	sticky := stickyKey(req.Messages)

	// 故障转移：最多试 3 个账号。
	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		id, err := s.Pool.Select(sticky)
		if err != nil {
			if lastErr != nil {
				break
			}
			s.writeErr(w, http.StatusServiceUnavailable, "no_healthy_account",
				"all accounts unavailable (cooldown/disabled). Run login or check /status")
			return
		}
		if tried[id] {
			break
		}
		tried[id] = true

		token, deviceID, terr := s.tokenFor(r.Context(), id)
		if terr != nil {
			s.Pool.Record(id, pool.Result{OK: false, Class: pool.ClassAuthDead, Message: terr.Error()})
			lastErr = terr
			continue
		}

		body, berr := s.Client.BuildBody(verdent.BuildParams{
			Model:       req.Model,
			Messages:    req.Messages,
			System:      "",
			ConvID:      sticky,
			MaxTokens:   req.MaxTokens,
			Temperature: req.Temperature,
			Stream:      true, // 上游始终用流式，本地按客户端需要聚合
			Tools:       req.Tools,
			ToolChoice:  req.ToolChoice,
		})
		if berr != nil {
			s.writeErr(w, http.StatusInternalServerError, "build_body_failed", berr.Error())
			return
		}

		resp, serr := s.Client.Stream(r.Context(), token, deviceID, body)
		if serr != nil {
			cls, msg := classifyUpstream(serr)
			s.Pool.Record(id, pool.Result{OK: false, Class: cls, Message: msg})
			lastErr = serr
			continue
		}
		// 成功拿到流：翻译并转发。
		defer resp.Body.Close()
		s.relay(w, resp, id, req.Model, req.Stream)
		return
	}
	// 全部失败。
	status, code := http.StatusBadGateway, "upstream_error"
	msg := "all attempts failed"
	if lastErr != nil {
		msg = lastErr.Error()
		if ue, ok := lastErr.(*verdent.UpstreamError); ok {
			status = mapStatus(ue.Status)
		}
	}
	s.writeErr(w, status, code, msg)
}

// tokenFor 取账号的可用 token 与设备ID，过期则 refresh 并回写 store。
func (s *Server) tokenFor(ctx context.Context, id string) (string, string, error) {
	acc, ok := s.Store.Get(id)
	if !ok {
		return "", "", fmt.Errorf("account gone: %s", id)
	}
	if acc.Valid() {
		dev, _ := s.Store.EnsureDeviceID(id)
		return acc.Token, dev, nil
	}
	// 尝试 refresh。
	refreshed, err := verdent.Refresh(ctx, s.HTTP, &acc)
	if err != nil {
		if acc.Token != "" {
			// refresh 失败但仍有旧 token，赌一把（可能仍有效）。
			dev, _ := s.Store.EnsureDeviceID(id)
			return acc.Token, dev, nil
		}
		return "", "", fmt.Errorf("token expired and refresh failed: %w", err)
	}
	_ = s.Store.UpdateTokens(id, refreshed.Token, refreshed.RefreshToken, refreshed.ExpireAtMS)
	dev, _ := s.Store.EnsureDeviceID(id)
	return refreshed.Token, dev, nil
}

// relay 翻译 hybrid-stream SSE 为 OpenAI 格式并转发。
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, id, model string, wantStream bool) {
	tr := verdent.NewTranslator(resp.Body, model)
	if wantStream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			s.writeErr(w, http.StatusInternalServerError, "no_flusher", "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		usage, err := tr.StreamTo(func(frame []byte) {
			_, _ = w.Write(frame)
			flusher.Flush()
		})
		if err != nil {
			// 流已开始，只能补错误帧。
			_, _ = w.Write(verdent.SSEErrorFrame(err.Error()))
			_, _ = w.Write(verdent.SSEDoneFrame())
			flusher.Flush()
			s.Pool.Record(id, pool.Result{OK: false, Class: pool.ClassTransient, Message: err.Error()})
			return
		}
		s.Pool.Record(id, pool.Result{OK: true, TokensIn: usage.In, TokensOut: usage.Out})
		return
	}
	// 非流式：聚合成一个 JSON。
	obj, usage, err := tr.Aggregate()
	if err != nil {
		s.Pool.Record(id, pool.Result{OK: false, Class: pool.ClassTransient, Message: err.Error()})
		s.writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj)
	s.Pool.Record(id, pool.Result{OK: true, TokensIn: usage.In, TokensOut: usage.Out})
}

// chatReq 解析关心的字段。
type chatReq struct {
	Model       string               `json:"model"`
	Messages    []verdent.OAIMessage `json:"messages"`
	Stream      bool                 `json:"stream"`
	MaxTokens   int                  `json:"max_tokens"`
	Temperature *float64             `json:"temperature"`
	Tools       json.RawMessage      `json:"tools"`
	ToolChoice  json.RawMessage      `json:"tool_choice"`
}

// stickyKey 从首条 user 消息派生会话粘性键。
func stickyKey(msgs []verdent.OAIMessage) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			return hashKey(s)
		}
		return hashKey(string(m.Content))
	}
	return ""
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:16])
}

// classifyUpstream 把上游错误映射到池分类。
func classifyUpstream(err error) (pool.Class, string) {
	ue, ok := err.(*verdent.UpstreamError)
	if !ok {
		return pool.ClassTransient, err.Error()
	}
	low := strings.ToLower(ue.Body)
	switch {
	case ue.Status == 401 || ue.Status == 403:
		return pool.ClassAuthDead, ue.Body
	case ue.Status == 402:
		return pool.ClassNoCredit, ue.Body
	case ue.Status == 429 || strings.Contains(low, "20004") || strings.Contains(low, "rate"):
		return pool.ClassRateLimited, ue.Body
	case strings.Contains(low, "insufficient") || strings.Contains(low, "quota") ||
		strings.Contains(low, "credit") || strings.Contains(low, "30001"):
		return pool.ClassNoCredit, ue.Body
	case ue.Status >= 500:
		return pool.ClassTransient, ue.Body
	}
	return pool.ClassOther, ue.Body
}

func mapStatus(upstream int) int {
	switch {
	case upstream == 401 || upstream == 403:
		return http.StatusUnauthorized
	case upstream == 402:
		return http.StatusPaymentRequired
	case upstream == 429:
		return http.StatusTooManyRequests
	case upstream >= 500:
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}
