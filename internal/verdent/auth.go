package verdent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// 逆向常量（桌面版 app.asar，与参考实现一致）。
const (
	wwwOrigin   = "https://www.verdent.ai"
	loginOrigin = "https://login.verdent.ai"
	userAgent   = "Verdent/2.15.1"
)

// PKCEChallenge 保存一次 PKCE 会话的校验参数。
type PKCEChallenge struct {
	Verifier  string
	Challenge string
	State     string
	DeviceID  string
}

// NewPKCE 生成一次 PKCE 挑战（code_verifier + S256 challenge + state + device id）。
func NewPKCE() (*PKCEChallenge, error) {
	verifier, err := b64urlRand(32)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, err := b64urlRand(32)
	if err != nil {
		return nil, err
	}
	dev, err := hexRand(16)
	if err != nil {
		return nil, err
	}
	return &PKCEChallenge{Verifier: verifier, Challenge: challenge, State: state, DeviceID: dev}, nil
}

// AuthorizeURL 构造浏览器登录 URL。
// 不能带 source=deck：登录页对 deck 来源只放行 localhost/127.0.0.1/0.0.0.0，
// 其它回调会被判无效并在 /auth 与 /login 之间反复跳转。
func (c *PKCEChallenge) AuthorizeURL(callback string) string {
	return fmt.Sprintf("%s/auth?challenge=%s&state=%s&intent=signin&callback=%s&ot=deck&source=pc&id=%s",
		wwwOrigin, urlEncode(c.Challenge), urlEncode(c.State), urlEncode(callback), urlEncode(c.DeviceID))
}

// pkceResp 是 /passport/pkce/callback 与 /passport/token/refresh 的响应壳。
type pkceResp struct {
	ErrCode int    `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
	Data    struct {
		Token                string      `json:"token"`
		AccessToken          string      `json:"accessToken"`
		AccessTokenAlt       string      `json:"access_token"`
		RefreshToken         string      `json:"refreshToken"`
		RefreshTokenAlt      string      `json:"refresh_token"`
		ExpireTime           json.Number `json:"expireTime"`
		AccessTokenExpiresAt json.Number `json:"accessTokenExpiresAt"`
		UserID               json.Number `json:"userId"`
	} `json:"data"`
}

// ExchangeCode 用授权码 + code_verifier 换 token，返回可入库的 Account。
func ExchangeCode(ctx context.Context, hc tls_client.HttpClient, code, verifier string) (*Account, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "codeVerifier": verifier})
	resp, err := postJSON(ctx, hc, loginOrigin+"/passport/pkce/callback", body)
	if err != nil {
		return nil, err
	}
	var pr pkceResp
	if err := json.Unmarshal(resp, &pr); err != nil {
		return nil, fmt.Errorf("unexpected pkce response: %s", truncate(string(resp), 200))
	}
	if pr.ErrCode != 0 {
		return nil, fmt.Errorf("pkce errCode=%d msg=%s", pr.ErrCode, pr.ErrMsg)
	}
	token := firstNonEmpty(pr.Data.Token, pr.Data.AccessToken, pr.Data.AccessTokenAlt)
	if token == "" {
		return nil, fmt.Errorf("pkce ok but no token in data")
	}
	expSec := firstNumber(pr.Data.AccessTokenExpiresAt, pr.Data.ExpireTime)
	dev, _ := hexRand(16)
	acc := &Account{
		UserID:       pr.Data.UserID.String(),
		Token:        token,
		RefreshToken: firstNonEmpty(pr.Data.RefreshToken, pr.Data.RefreshTokenAlt),
		DeviceID:     dev,
		ExpireAtMS:   expSec * 1000,
		ObtainedAtMS: time.Now().UnixMilli(),
	}
	return acc, nil
}

// Refresh 用 refresh_token 换新的 access token（refresh token 会轮换）。
func Refresh(ctx context.Context, hc tls_client.HttpClient, a *Account) (*Account, error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return nil, fmt.Errorf("no refresh token")
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": a.RefreshToken})
	resp, err := postJSON(ctx, hc, loginOrigin+"/passport/token/refresh", body)
	if err != nil {
		return nil, err
	}
	var pr pkceResp
	if err := json.Unmarshal(resp, &pr); err != nil {
		return nil, fmt.Errorf("unexpected refresh response: %s", truncate(string(resp), 200))
	}
	if pr.ErrCode != 0 {
		return nil, fmt.Errorf("refresh errCode=%d msg=%s", pr.ErrCode, pr.ErrMsg)
	}
	token := firstNonEmpty(pr.Data.AccessToken, pr.Data.AccessTokenAlt, pr.Data.Token)
	if token == "" {
		return nil, fmt.Errorf("refresh ok but no access token")
	}
	out := *a
	out.Token = token
	expSec := firstNumber(pr.Data.AccessTokenExpiresAt, pr.Data.ExpireTime)
	out.ExpireAtMS = expSec * 1000
	if rt := firstNonEmpty(pr.Data.RefreshToken, pr.Data.RefreshTokenAlt); rt != "" {
		out.RefreshToken = rt
	}
	return &out, nil
}

func postJSON(ctx context.Context, hc tls_client.HttpClient, url string, body []byte) ([]byte, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func b64urlRand(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hexRand(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNumber(ns ...json.Number) int64 {
	for _, n := range ns {
		if n == "" {
			continue
		}
		if v, err := n.Int64(); err == nil && v != 0 {
			return v
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
