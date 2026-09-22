// Package server 暴露 OpenAI 兼容接口，把请求路由到账号池选出的上游 key。
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
	"verdent2api/internal/upstream"
)

// Server HTTP 网关。
type Server struct {
	Pool           *pool.Pool
	Client         *upstream.Client
	APIKey         string // 网关侧鉴权 key；空表示不鉴权
	ModelMap       map[string]string
	ModelAlias     map[string]string
	StripReasoning bool
	Version        string
}

// Register 注册路由（全部走网关侧鉴权）。
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
		if s.APIKey != "" {
			got := bearerToken(r)
			if got != s.APIKey {
				s.writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
					"Incorrect API key provided. Check the API key you provided.")
				return
			}
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return h
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.writeOpenAIError(w, http.StatusNotFound, "not_found",
		"Unknown endpoint: "+r.URL.Path)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"ts":     time.Now().Unix(),
	})
}

// models 透传上游模型列表，叠加网关别名。
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	keys := s.Pool.Snapshot()
	if len(keys) == 0 {
		// 无 key 时仍返回别名表，便于客户端配置。
		s.writeModels(w, nil)
		return
	}
	// 探活用第一个非禁用 key 拉一次列表；失败不进入状态机
	// （真实请求才做错误分类，避免探活把健康 key 误禁）。
	var data []upstream.Model
	for _, st := range keys {
		if st.Disabled {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		resp, err := s.Client.Models(ctx, st.APIKey)
		cancel()
		if err == nil && resp != nil {
			data = resp.Data
			break
		}
	}
	s.writeModels(w, data)
}

func (s *Server) writeModels(w http.ResponseWriter, data []upstream.Model) {
	seen := make(map[string]bool)
	out := make([]upstream.Model, 0, len(data)+len(s.ModelAlias))
	for _, m := range data {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	// 网关别名也作为可用模型暴露。
	for alias := range s.ModelAlias {
		if !seen[alias] {
			seen[alias] = true
			out = append(out, upstream.Model{ID: alias, Object: "model", OwnedBy: "verdent2api"})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   out,
	})
}

// chatRequest 仅用于解析关心的字段；完整请求体原样透传，不丢字段。
type chatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`
}

// chatCompletions 主链路：选号 → 透传 → 规范化流。
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s.writeOpenAIError(w, http.StatusBadRequest, "bad_request", "read body failed")
		return
	}

	// 解析为通用 map：既取必要字段，又保留原始 body 透传。
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeOpenAIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	modelRaw, ok := req["model"]
	if !ok {
		s.writeOpenAIError(w, http.StatusBadRequest, "bad_request", "missing field: model")
		return
	}
	var modelName string
	_ = json.Unmarshal(modelRaw, &modelName)
	var streamFlag bool
	if v, ok := req["stream"]; ok {
		_ = json.Unmarshal(v, &streamFlag)
	}

	// 别名/映射：客户端别名 → 上游真实模型名。
	upModel := s.resolveModel(modelName)
	if upModel != modelName {
		req["model"], _ = json.Marshal(upModel)
		body, _ = json.Marshal(req)
	}

	// 故障转移：选号 → 请求；若在响应头到达前失败且错误可重试
	// （429 / 5xx / 网络错误），换下一个健康 key 再试，直到用尽或成功。
	// 一旦响应头返回（流已开始）就不再重试，避免向客户端重复写数据。
	res, apiKey, err := s.dispatch(r, body, streamFlag)
	if err != nil {
		ue, isUp := err.(*upstream.Error)
		if isUp {
			s.writeOpenAIError(w, mapStatus(ue), string(ue.Class), ue.RawBody)
		} else if err == pool.ErrNoHealthy {
			s.writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
				"all accounts are unavailable (cooldown / disabled). Check /status")
		} else {
			s.writeOpenAIError(w, http.StatusBadGateway, "upstream_unreachable", err.Error())
		}
		return
	}
	defer res.Body.Close()
	defer s.Pool.Release(apiKey)

	if streamFlag {
		s.streamResponse(w, r, res, apiKey, upModel)
	} else {
		s.proxyResponse(w, res, apiKey)
	}
}

// maxDispatchAttempts 单次请求最多尝试的 key 数（含首选）。
const maxDispatchAttempts = 3

// dispatch 选号并请求上游，可重试错误时自动换号。
// 成功返回的 (res, apiKey) 已持有一个在途租约，调用方必须 Release(apiKey)。
// 失败时租约已释放；错误分类已记入状态机。
func (s *Server) dispatch(r *http.Request, body []byte, streamFlag bool) (*upstream.ChatResult, string, error) {
	sticky := stickyKey(reqMap(body))
	tried := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxDispatchAttempts; attempt++ {
		apiKey, err := s.Pool.Select(r.Context(), sticky)
		if err != nil {
			if lastErr != nil {
				return nil, "", lastErr
			}
			return nil, "", err
		}
		// 同一 key 已试过（粘性命中或池太小），不再重复占用。
		if tried[apiKey] {
			if lastErr != nil {
				return nil, "", lastErr
			}
			return nil, "", err
		}
		tried[apiKey] = true

		s.Pool.Acquire(apiKey)
		res, cerr := s.Client.ChatCompletions(r.Context(), apiKey, body, streamFlag)
		if cerr == nil {
			return res, apiKey, nil // 租约留给调用方释放
		}
		s.Pool.Release(apiKey)

		if ue, ok := cerr.(*upstream.Error); ok {
			s.Pool.Record(apiKey, pool.Result{OK: false, Class: ue.Class, Message: ue.RawBody})
			lastErr = cerr
			// key 失效或缺额：换号重试仍有意义；限流/5xx 亦然。
			// 仅当分类不可通过换号缓解（理论上没有）才停止——这里一律换号。
			continue
		}
		// 网络层错误（超时/连接失败）：记为 transient 并换号。
		s.Pool.Record(apiKey, pool.Result{OK: false, Class: upstream.ClassTransient, Message: cerr.Error()})
		lastErr = cerr
	}
	return nil, "", lastErr
}

// reqMap 解析 body 为字段 map（供派生粘性键用），失败返回 nil。
func reqMap(body []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	return m
}

// resolveModel 客户端模型名 → 上游模型名。
func (s *Server) resolveModel(name string) string {
	if v, ok := s.ModelAlias[name]; ok && v != "" {
		return v
	}
	if v, ok := s.ModelMap[name]; ok && v != "" {
		return v
	}
	return name
}

// stickyKey 从请求体派生会话粘性键。
// 优先级：conversation_id 系列字段 → prompt_cache_key → 首条 user 消息 sha256。
func stickyKey(req map[string]json.RawMessage) string {
	for _, k := range []string{"conversation_id", "conversationId", "metadata"} {
		if v, ok := req[k]; ok {
			if s := extractStringField(v, k); s != "" {
				return hashKey(s)
			}
		}
	}
	if v, ok := req["prompt_cache_key"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			return hashKey("pck:" + s)
		}
	}
	// 首条 user 消息兜底：会话内历史追加不影响该键。
	if msgs, ok := req["messages"]; ok {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(msgs, &arr) == nil {
			for _, m := range arr {
				var role string
				_ = json.Unmarshal(m["role"], &role)
				if role != "user" {
					continue
				}
				var content string
				if err := json.Unmarshal(m["content"], &content); err == nil && content != "" {
					return hashKey(content)
				}
				// content 为数组（多模态）时取首个文本块。
				var parts []map[string]interface{}
				if json.Unmarshal(m["content"], &parts) == nil {
					for _, p := range parts {
						if t, ok := p["type"].(string); ok && t == "text" {
							if txt, ok := p["text"].(string); ok && txt != "" {
								return hashKey(txt)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func extractStringField(raw json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"conversation_id", "conversationId"} {
			if v, ok := m[k]; ok {
				_ = json.Unmarshal(v, &s)
				if s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:16])
}

// streamResponse 逐帧规范化并转发 SSE。
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, res *upstream.ChatResult, apiKey, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeOpenAIError(w, http.StatusInternalServerError, "no_flusher", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := upstream.NewChunkID()
	created := time.Now().Unix()
	reader := upstream.NewSSEReader(res.Body)

	var usage *upstream.Usage
	for {
		data, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 流中断：补一个错误帧再结束，保证客户端拿到 [DONE]。
			_, _ = w.Write(upstream.ErrorFrame("stream interrupted: " + err.Error()))
			_, _ = w.Write(upstream.DoneFrame())
			flusher.Flush()
			s.Pool.Record(apiKey, pool.Result{OK: false, Class: upstream.ClassTransient, Message: err.Error()})
			return
		}
		if len(data) == 0 {
			continue
		}
		chunk, perr := upstream.ParseChunk(data)
		if perr != nil {
			// 非 JSON 帧（如上游自定义事件）原样透传。
			_, _ = w.Write(upstream.Frame(data))
			flusher.Flush()
			continue
		}
		if chunk != nil && chunk.Usage != nil {
			usage = chunk.Usage
		}
		out := upstream.Normalize(chunk, model, id, created)
		if s.StripReasoning {
			out = stripReasoning(out)
		}
		_, _ = w.Write(upstream.Frame(out))
		flusher.Flush()
	}
	_, _ = w.Write(upstream.DoneFrame())
	flusher.Flush()

	s.Pool.Record(apiKey, pool.Result{OK: true, Usage: usage})
}

// stripReasoning 移除 reasoning_content 字段（部分客户端不识别）。
func stripReasoning(payload []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	changed := false
	if raw, ok := m["choices"]; ok {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			for i := range arr {
				var d map[string]json.RawMessage
				if json.Unmarshal(arr[i]["delta"], &d) != nil {
					continue
				}
				if _, has := d["reasoning_content"]; has {
					delete(d, "reasoning_content")
					delete(d, "reasoning")
					nb, _ := json.Marshal(d)
					arr[i]["delta"] = nb
					changed = true
				}
			}
			if changed {
				nb, _ := json.Marshal(arr)
				m["choices"] = nb
			}
		}
	}
	if !changed {
		return payload
	}
	out, _ := json.Marshal(m)
	return out
}

// proxyResponse 非流式：原样透传上游 body（不改字段顺序），旁路解析 usage 记账。
func (s *Server) proxyResponse(w http.ResponseWriter, res *upstream.ChatResult, apiKey string) {
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		s.Pool.Record(apiKey, pool.Result{OK: false, Class: upstream.ClassTransient, Message: err.Error()})
		s.writeOpenAIError(w, http.StatusBadGateway, "upstream_read_failed", err.Error())
		return
	}
	// 旁路取 usage，不影响透传字节。
	var usage *upstream.Usage
	var payload struct {
		Usage *upstream.Usage `json:"usage"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Usage != nil {
		usage = payload.Usage
	}
	w.Header().Set("Content-Type", pickContentType(res.ContentType))
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(raw)
	s.Pool.Record(apiKey, pool.Result{OK: true, Usage: usage})
}

func pickContentType(in string) string {
	if in != "" {
		return in
	}
	return "application/json"
}

func mapStatus(e *upstream.Error) int {
	switch e.Class {
	case upstream.ClassKeyDead:
		return http.StatusUnauthorized
	case upstream.ClassNoCredit:
		return http.StatusPaymentRequired
	case upstream.ClassRateLimited:
		return http.StatusTooManyRequests
	case upstream.ClassTransient:
		return http.StatusBadGateway
	}
	if e.StatusCode > 0 {
		return e.StatusCode
	}
	return http.StatusBadGateway
}
