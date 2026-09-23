// cmd/login 是桌面端 PKCE 登录工具。
//
// 流程与 Verdent 桌面 App 一致：
//  1. 生成本地回调端口
//  2. 打印（并尝试打开）www.verdent.ai/auth 的 PKCE 登录 URL
//  3. 浏览器登录后回调 ?code=
//  4. POST login.verdent.ai/passport/pkce/callback 换 token
//  5. 写入账号文件，网关 15s 内热重载
//
//	./verdent-login
//	./verdent-login -auth ./auths/accounts.json
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"verdent2api/internal/verdent"
)

func main() {
	authFile := flag.String("auth", "./auths/accounts.json", "账号凭证文件")
	label := flag.String("label", "", "账号备注")
	timeout := flag.Duration("timeout", 5*time.Minute, "等待登录超时")
	flag.Parse()

	store, err := verdent.NewStore(*authFile)
	if err != nil {
		log.Fatalf("auth store: %v", err)
	}
	pkce, err := verdent.NewPKCE()
	if err != nil {
		log.Fatalf("pkce: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		select {
		case codeCh <- code:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<h2>登录已捕获，可以关掉这个标签页。</h2>"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	callback := fmt.Sprintf("http://%s/auth/callback", ln.Addr().String())
	url := pkce.AuthorizeURL(callback)
	fmt.Println()
	fmt.Println("打开下面的链接，用 Verdent 账号登录：")
	fmt.Println()
	fmt.Println("  " + url)
	fmt.Println()
	openBrowser(url)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(*timeout):
		log.Fatal("login timed out")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	acc, err := verdent.ExchangeCode(ctx, &http.Client{Timeout: 30 * time.Second}, code, pkce.Verifier)
	if err != nil {
		log.Fatalf("exchange code: %v", err)
	}
	if *label != "" {
		acc.Label = *label
	}
	if err := store.Upsert(*acc); err != nil {
		log.Fatalf("save account: %v", err)
	}
	log.Printf("login ok, saved to %s (user=%s)", *authFile, acc.UserID)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
