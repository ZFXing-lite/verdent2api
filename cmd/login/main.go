// cmd/login 是桌面端 PKCE 登录工具。
//
// 流程与 Verdent 桌面 App 一致：
//
//  1. 生成本地回调端口
//
//  2. 打印（并尝试打开）www.verdent.ai/auth 的 PKCE 登录 URL
//
//  3. 浏览器登录后回调 ?code=
//
//  4. POST login.verdent.ai/passport/pkce/callback 换 token
//
//  5. 写入账号文件，网关 15s 内热重载
//
//     ./verdent-login
//     ./verdent-login -auth ./auths/accounts.json
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
	"strings"
	"time"

	"verdent2api/internal/verdent"
)

func main() {
	authFile := flag.String("auth", "./auths/accounts.json", "账号凭证文件")
	label := flag.String("label", "", "账号备注")
	listen := flag.String("listen", "0.0.0.0:17867", "回调监听地址，默认所有网卡的固定端口")
	public := flag.String("public", "", "写进登录链接的回调基址，如 http://1.2.3.4:17867；空则自动用宿主机 IP")
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		log.Fatalf("callback addr: %v", err)
	}
	base := strings.TrimRight(strings.TrimSpace(*public), "/")
	if base == "" {
		ip := host
		if ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]" {
			ip = hostIP()
		}
		base = "http://" + net.JoinHostPort(ip, port)
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

	callback := base + "/auth/callback"
	url := pkce.AuthorizeURL(callback)
	fmt.Println()
	fmt.Println("回调地址（登录成功后落盘到本机）：")
	fmt.Println()
	fmt.Println("  " + callback)
	fmt.Println()
	fmt.Println("打开下面的链接，用 Verdent 账号登录。任意设备打开都可以，回调会回到这台机器：")
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

func hostIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatalf("detect host ip: %v (pass -public http://<宿主机IP>:17867)", err)
	}
	defer conn.Close()
	ip, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil || ip == "" {
		log.Fatalf("detect host ip: %v", err)
	}
	return ip
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
