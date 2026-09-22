// Package platform 封装 platform.verdent.ai 控制台接口。
//
// 逆向事实：
//   - 登录域：https://login.verdent.ai，POST /passport/login（body: email/password/token）
//     token 是 Cloudflare Turnstile 人机验证 token，站点密钥 0x4AAAAAABg0OrUFnV_hhAeU
//   - 未带有效 token 返回 {"errCode":100028,"errMsg":"illegal request"}
//   - 无效 token 返回 {"errCode":100003,"errMsg":"invalid token"}
//   - 业务响应统一为 {"errCode":0,"errMsg":"","data":...}，非 0 表示失败
//   - 用户态：GET  https://platform.verdent.ai/api/verdent/home  （含 is_login / teams）
//   - 建键：  POST https://platform.verdent.ai/api/verdent/team/api-keys/create
package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 控制台客户端（携带登录 cookie 调接口）。
type Client struct {
	Platform string
	Login    string
	HTTP     *http.Client
}

// NewClient 构造客户端。
func NewClient(platformOrigin, loginOrigin string) *Client {
	platformOrigin = strings.TrimRight(strings.TrimSpace(platformOrigin), "/")
	loginOrigin = strings.TrimRight(strings.TrimSpace(loginOrigin), "/")
	if platformOrigin == "" {
		platformOrigin = "https://platform.verdent.ai"
	}
	if loginOrigin == "" {
		loginOrigin = "https://login.verdent.ai"
	}
	return &Client{
		Platform: platformOrigin,
		Login:    loginOrigin,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			Jar:     nil, // 由调用方管理 cookie 串
		},
	}
}

// envelope 平台统一响应壳。
type envelope struct {
	ErrCode int             `json:"errCode"`
	ErrMsg  string          `json:"errMsg"`
	Data    json.RawMessage `json:"data"`
	Details interface{}     `json:"details"`
}

// apiError 平台业务错误。
type apiError struct {
	Code int
	Msg  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("platform errCode=%d msg=%s", e.Code, e.Msg)
}

// PassportLogin 用邮箱密码 + Turnstile token 换登录 cookie。
// 成功返回可复用的 cookie 串（"name=value; name2=value2"）。
func (c *Client) PassportLogin(ctx context.Context, email, password, turnstileToken string) (string, error) {
	body := map[string]string{
		"email":    email,
		"password": password,
		"token":    turnstileToken,
	}
	req, err := c.newJSON(ctx, http.MethodPost, c.Login+"/passport/login", body)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		// 非 JSON（如 HTML 错误页）说明路径/域不对。
		return "", fmt.Errorf("unexpected login response (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if env.ErrCode != 0 {
		return "", &apiError{Code: env.ErrCode, Msg: env.ErrMsg}
	}
	// 收集 Set-Cookie。
	var cookies []string
	for _, ck := range resp.Cookies() {
		if ck.Value == "" {
			continue
		}
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	if len(cookies) == 0 {
		return "", fmt.Errorf("login ok but no cookie returned")
	}
	return strings.Join(cookies, "; "), nil
}

// HomeResponse /api/verdent/home 的 data 部分（只取需要的字段）。
type HomeResponse struct {
	IsLogin  bool       `json:"is_login"`
	UserInfo *UserInfo  `json:"user_info"`
	Teams    []TeamInfo `json:"teams"`
}

type UserInfo struct {
	ID    json.Number `json:"id"`
	Name  string      `json:"name"`
	Email string      `json:"email"`
}

type TeamInfo struct {
	TeamID   json.Number `json:"team_id"`
	TeamName string      `json:"team_name"`
}

// FirstTeamID 返回首个可用团队 id（个人账号通常也有一个个人团队）。
func (h *HomeResponse) FirstTeamID() string {
	if h == nil {
		return ""
	}
	for _, t := range h.Teams {
		if s := t.TeamID.String(); s != "" && s != "0" {
			return s
		}
	}
	// teams 为空时退回 user_id（部分账号把个人空间当默认团队）。
	if h.UserInfo != nil {
		if s := h.UserInfo.ID.String(); s != "" && s != "0" {
			return s
		}
	}
	return ""
}

// Home 拉取用户态（含是否登录与团队列表）。
func (c *Client) Home(ctx context.Context, cookie string) (*HomeResponse, error) {
	var out HomeResponse
	if err := c.getJSON(ctx, c.Platform+"/api/verdent/home", cookie, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateKeyRequest 建键请求体（字段名取自平台前端）。
type CreateKeyRequest struct {
	Name   string
	TeamID string
	Models []string
	// 可选：到期时间戳（秒），0 表示永不过期。
	ExpiresAt int64
}

// CreateAPIKey 创建一个 api.verdent.ai 的 API key，返回明文（仅此一次可见）。
func (c *Client) CreateAPIKey(ctx context.Context, cookie string, req CreateKeyRequest) (string, error) {
	if len(req.Models) == 0 {
		req.Models = []string{"*"}
	}
	payload := map[string]interface{}{
		"name":            req.Name,
		"team_id":         req.TeamID,
		"scope":           map[string]interface{}{"models": req.Models},
		"expires_at":      req.ExpiresAt,
		"idempotency_key": idempotencyKey(),
	}
	b, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Platform+"/api/verdent/team/api-keys/create", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if cookie != "" {
		httpReq.Header.Set("Cookie", cookie)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("unexpected create-key response (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if env.ErrCode != 0 {
		return "", &apiError{Code: env.ErrCode, Msg: env.ErrMsg}
	}
	var data struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.APIKey == "" {
		return "", fmt.Errorf("create-key ok but no api_key in data: %s", truncate(string(env.Data), 200))
	}
	return data.APIKey, nil
}

// ListAPIKeys 列出当前团队的 key（脱敏信息，用于诊断）。
func (c *Client) ListAPIKeys(ctx context.Context, cookie, teamID string) ([]map[string]interface{}, error) {
	var data struct {
		List  []map[string]interface{} `json:"list"`
		Total int                      `json:"total"`
		Page  int                      `json:"page"`
	}
	q := fmt.Sprintf("page=1&page_size=50&team_id=%s", teamID)
	if err := c.getJSON(ctx, c.Platform+"/api/verdent/team/api-keys/list?"+q, cookie, &data); err != nil {
		return nil, err
	}
	return data.List, nil
}

// getJSON GET 一个平台 JSON 接口并解包 data 字段。
func (c *Client) getJSON(ctx context.Context, url, cookie string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unexpected response from %s (HTTP %d): %s", url, resp.StatusCode, truncate(string(raw), 200))
	}
	if env.ErrCode != 0 {
		return &apiError{Code: env.ErrCode, Msg: env.ErrMsg}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (c *Client) newJSON(ctx context.Context, method, url string, payload interface{}) (*http.Request, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// idempotencyKey 生成平台要求的幂等键。
func idempotencyKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
