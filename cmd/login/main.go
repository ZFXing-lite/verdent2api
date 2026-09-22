// cmd/login 是 verdent2api 的登录工具。
//
// 交互流程（Turnstile 必须人工完成，无法离线绕过）：
//  1. 本地起一个 HTTP 服务，渲染带 Cloudflare Turnstile 的登录页
//     （站点密钥逆向自 cloud.verdent.ai 前端：0x4AAAAAABg0OrUFnV_hhAeU）
//  2. 用户在浏览器输入邮箱/密码并完成人机验证
//  3. 工具拿 turnstile token 调 https://login.verdent.ai/passport/login
//  4. 用登录态 cookie 调 platform 控制台创建 api.verdent.ai 的 API key
//  5. 把 key 追加写入凭证文件（供网关账号池加载）
//
// 用法：
//
//	./verdent-login                       # 交互式（默认 :17867）
//	./verdent-login -listen :17867 -keys ./auths/verdent-keys.json
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"verdent2api/internal/auth"
	"verdent2api/internal/platform"
)

// 逆向得到的常量（生产环境）。
const (
	defaultListen    = ":17867"
	turnstileSiteKey = "0x4AAAAAABg0OrUFnV_hhAeU"
	loginOrigin      = "https://login.verdent.ai"
	platformOrigin   = "https://platform.verdent.ai"
)

func main() {
	listen := flag.String("listen", defaultListen, "本地登录服务监听地址")
	keyFile := flag.String("keys", "./auths/verdent-keys.json", "API key 凭证文件（成功后写入）")
	keyName := flag.String("name", "", "API key 备注名（默认自动生成）")
	flag.Parse()

	store, err := auth.New(*keyFile)
	if err != nil {
		log.Fatalf("load key store: %v", err)
	}
	pc := platform.NewClient(platformOrigin, loginOrigin)

	mux := http.NewServeMux()
	srv := &loginServer{
		store:    store,
		platform: pc,
		keyName:  *keyName,
		keyFile:  *keyFile,
	}

	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/login", srv.handleLogin)
	mux.HandleFunc("/api/status", srv.handleStatus)
	// Cloudflare Turnstile 资源反向代理：
	// 国内浏览器直连 challenges.cloudflare.com 常失败，走本代理后
	// 源 IP 变成本机（国外），且浏览器指纹仍是用户真实浏览器，可正常过验证。
	mux.HandleFunc("/__cf/", srv.handleCFProxy)

	log.Printf("verdent2api login tool listening on http://127.0.0.1%s", *listen)
	log.Printf("open the URL in a browser, sign in, and the API key will be saved to %s", *keyFile)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("login server: %v", err)
	}
}

type loginServer struct {
	store    *auth.Store
	platform *platform.Client
	keyName  string
	keyFile  string
}

func (s *loginServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

// loginRequest 前端提交的登录表单。
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Token    string `json:"token"` // Turnstile token
}

// loginResponse 回给前端的结果。
type loginResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	APIKey  string `json:"api_key,omitempty"`
	Label   string `json:"label,omitempty"`
}

func (s *loginServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, loginResponse{Message: "POST required"})
		return
	}
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, loginResponse{Message: "invalid body"})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" || req.Token == "" {
		writeJSON(w, http.StatusBadRequest, loginResponse{Message: "email / password / turnstile token are all required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// 1) passport 登录（拿 cookie）。
	cookies, err := s.platform.PassportLogin(ctx, req.Email, req.Password, req.Token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, loginResponse{Message: "login failed: " + err.Error()})
		return
	}

	// 2) 读 home 拿 teams + 校验登录态。
	home, err := s.platform.Home(ctx, cookies)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, loginResponse{Message: "fetch home failed: " + err.Error()})
		return
	}
	if !home.IsLogin {
		writeJSON(w, http.StatusUnauthorized, loginResponse{Message: "session not established after login (is_login=false)"})
		return
	}
	teamID := home.FirstTeamID()
	if teamID == "" {
		writeJSON(w, http.StatusBadGateway, loginResponse{Message: "no team found for this account; create a team in the console first"})
		return
	}

	// 3) 创建 API key。
	name := s.keyName
	if name == "" {
		name = "verdent2api-" + time.Now().Format("0102-1504")
	}
	key, err := s.platform.CreateAPIKey(ctx, cookies, platform.CreateKeyRequest{
		Name:   name,
		TeamID: teamID,
		Models: []string{"*"},
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, loginResponse{Message: "create api key failed: " + err.Error()})
		return
	}

	// 4) 落盘。
	label := name
	if err := s.store.Add(auth.Entry{APIKey: key, Label: label, CreatedBy: req.Email}); err != nil {
		// key 已存在也把明文回给用户，避免白创建一次。
		writeJSON(w, http.StatusConflict, loginResponse{OK: true, APIKey: key, Label: label, Message: "created but not saved: " + err.Error()})
		return
	}
	log.Printf("api key created and saved: label=%q team_id=%s", label, teamID)
	writeJSON(w, http.StatusOK, loginResponse{OK: true, APIKey: key, Label: label, Message: "saved to " + s.keyFile})
}

func (s *loginServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	keys := s.store.Keys()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keys":  len(keys),
		"file":  s.keyFile,
		"store": maskList(keys),
	})
}

// cfProxyTarget 反向代理目标。
const cfProxyTarget = "https://challenges.cloudflare.com"

// handleCFProxy 反向代理 /__cf/<path> -> https://challenges.cloudflare.com/<path>
// 透传方法、query、body 与部分头，供 Turnstile widget 在国内网络下加载。
func (s *loginServer) handleCFProxy(w http.ResponseWriter, r *http.Request) {
	// 剥离 /__cf 前缀，保留后续路径与 query。
	rel := strings.TrimPrefix(r.URL.Path, "/__cf")
	if rel == "" || rel == "/" {
		http.Error(w, "bad proxy path", http.StatusBadRequest)
		return
	}
	target := cfProxyTarget + rel
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 透传客户端头，但去掉 host 与可能的压缩头（由本服务决定）。
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			proxyReq.Header.Add(k, v)
		}
	}
	proxyReq.Header.Set("Accept-Encoding", "identity")
	proxyReq.Header.Set("Referer", "https://challenges.cloudflare.com/")

	resp, err := cfProxyClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "proxy failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 透传响应头（Content-Type / CSP 等），但移除可能破坏本页的压缩编码声明；
	// 并把重定向 Location 重新拉回 /__cf 前缀，避免跟随 302 时跳出代理。
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			if strings.EqualFold(k, "Location") {
				v = rewriteLocation(v)
			}
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// rewriteLocation 把上游重定向地址映射回 /__cf 代理路径。
// 支持绝对 URL（https://challenges.cloudflare.com/x）与相对路径（/x）。
func rewriteLocation(loc string) string {
	if loc == "" {
		return loc
	}
	const CF = "https://challenges.cloudflare.com"
	if strings.HasPrefix(loc, CF) {
		return "/__cf" + strings.TrimPrefix(loc, CF)
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc // 其它域的重定向不接管
	}
	if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "/__cf") {
		return "/__cf" + loc
	}
	return loc
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func maskList(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(k) <= 12 {
			out = append(out, "***")
			continue
		}
		out = append(out, k[:6]+"..."+k[len(k)-4:])
	}
	return out
}

// idempotencyKey 生成幂等键（平台要求）。
func idempotencyKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cfProxyClient 反向代理 Cloudflare 专用 HTTP 客户端（超时宽松，禁重定向跟随）。
var cfProxyClient = &http.Client{
	Timeout: 0,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
