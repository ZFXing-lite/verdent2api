// Package upstream 是 api.verdent.ai（Verdent 官方 OpenAI 兼容网关）的客户端。
//
// 协议事实（逆向自 cloud.verdent.ai / platform.verdent.ai 前端）：
//   - 上游是标准 OpenAI 兼容服务，Bearer 鉴权：
//     GET  https://api.verdent.ai/v1/models
//     POST https://api.verdent.ai/v1/chat/completions
//   - 未授权返回 {"type":"error","error":{"type":"authentication_error","message":"..."}}
//   - key 由 platform.verdent.ai 控制台创建：POST /api/verdent/team/api-keys/create
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
)

// DefaultBaseURL 生产环境上游地址。
const DefaultBaseURL = "https://api.verdent.ai"

// Client 上游 HTTP 客户端。连接池复用，超时可配。
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// UserAgent 出站 UA；空则用默认值。
	UserAgent string
}

// New 构造客户端，timeout 为整体请求超时，headerTimeout 为首字节超时。
func New(baseURL string, timeout, headerTimeout, idleTimeout time.Duration, ua string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	t := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     orDefault(idleTimeout, 300*time.Second),
		// ResponseHeaderTimeout 控制流式首字节等待，避免 hang 死。
		ResponseHeaderTimeout: orDefault(headerTimeout, 120*time.Second),
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		BaseURL:   baseURL,
		HTTP:      &http.Client{Transport: t, Timeout: orDefault(timeout, 120*time.Second)},
		UserAgent: ua,
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Model 上游 /v1/models 单项（OpenAI 标准）。
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ModelsResponse OpenAI 标准 models 响应。
type ModelsResponse struct {
	Object string  `json:"object,omitempty"`
	Data   []Model `json:"data"`
}

// Models 拉取上游模型列表。
func (c *Client) Models(ctx context.Context, apiKey string) (*ModelsResponse, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req, apiKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, apiKey); err != nil {
		return nil, err
	}
	var out ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return &out, nil
}

// ChatResult 一次 chat 透传的结果。Body 由调用方负责关闭。
type ChatResult struct {
	StatusCode  int
	Body        io.ReadCloser
	ContentType string
}

// ChatCompletions 透传 /v1/chat/completions。body 为已构造好的 JSON（含 model 映射后的值）。
// stream=true 时 Body 是原始 SSE 流，调用方逐帧转发。
func (c *Client) ChatCompletions(ctx context.Context, apiKey string, body []byte, stream bool) (*ChatResult, error) {
	req, err := c.newReq(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.auth(req, apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		// 明确要求 SSE，某些上游在缺 Accept 时会退化为非流式。
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, apiKey); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return &ChatResult{
		StatusCode:  resp.StatusCode,
		Body:        resp.Body,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func (c *Client) newReq(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ua := strings.TrimSpace(c.UserAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	} else {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	return req, nil
}

func (c *Client) auth(req *http.Request, apiKey string) {
	if k := strings.TrimSpace(apiKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
}

// checkStatus 把非 2xx 响应转成带分类的 Error，并消费完 body。
func checkStatus(resp *http.Response, apiKey string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg := strings.TrimSpace(string(raw))
	// 尝试按 OpenAI 错误格式提取 message。
	if m := extractOpenAIErrorMessage(raw); m != "" {
		msg = m
	}
	return &Error{
		StatusCode: resp.StatusCode,
		RawBody:    msg,
		Class:      classify(resp.StatusCode, msg),
		KeyHint:    keyHint(apiKey),
	}
}

// Error 带分类的上游错误。
type Error struct {
	StatusCode int
	RawBody    string
	Class      Class
	KeyHint    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream status %d [%s]: %s", e.StatusCode, e.Class, truncate(e.RawBody, 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func keyHint(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 12 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}

// extractOpenAIErrorMessage 从 {"error":{"message":..}} 或
// {"type":"error","error":{"message":..}} 中取 message。
func extractOpenAIErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v1 struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &v1) == nil && v1.Error.Message != "" {
		return v1.Error.Message
	}
	var v2 struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &v2) == nil && v2.Message != "" {
		return v2.Message
	}
	if v2.Error != "" {
		return v2.Error
	}
	return ""
}
