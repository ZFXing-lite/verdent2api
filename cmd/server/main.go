// cmd/server 是 verdent2api 网关。
//
// 上游是桌面端真实链路 llm-proxy.verdent.ai/llm/stream：
// PKCE 登录 token + AES-256-GCM 加密信封，命中 Free mode 限免模型。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"verdent2api/internal/pool"
	"verdent2api/internal/server"
	"verdent2api/internal/verdent"
)

const version = "2.0.0"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	store, err := verdent.NewStore(cfg.AuthFile)
	if err != nil {
		log.Fatalf("auth store: %v", err)
	}
	if n := len(store.IDs()); n == 0 {
		log.Printf("warning: no account in %s; run verdent-login first", cfg.AuthFile)
	}

	client := verdent.NewClient(
		cfg.Upstream.BaseURL,
		cfg.Upstream.TimeoutSeconds*time.Second,
		cfg.Upstream.HeaderTimeoutSeconds*time.Second,
		cfg.Upstream.IdleTimeoutSeconds*time.Second,
	)
	p := pool.New(pool.Config{
		ErrThreshold:   cfg.Pool.ErrThreshold,
		ErrCooldown:    cfg.Pool.ErrCooldown,
		RateCooldown:   cfg.Pool.RateCooldown,
		CreditCooldown: cfg.Pool.CreditCooldown,
	}, store, cfg.StateFile)

	catalog := verdent.NewCatalog(cfg.CatalogFile)
	srv := &server.Server{
		Pool:     p,
		Store:    store,
		Client:   client,
		Catalog:  catalog,
		APIKey:   cfg.APIKey,
		FreeOnly: cfg.FreeOnly,
		Version:  version,
		HTTP:     verdent.NewFingerprintClient(30 * time.Second),
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go runReloader(store, p)

	go func() {
		log.Printf("verdent2api %s listening on http://127.0.0.1%s (upstream %s)",
			version, cfg.Listen, client.Base)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func runReloader(store *verdent.Store, p *pool.Pool) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	prev := strings.Join(store.IDs(), "\x00")
	for range ticker.C {
		if err := store.Reload(); err != nil {
			log.Printf("auth reload failed: %v", err)
			continue
		}
		sig := strings.Join(store.IDs(), "\x00")
		if sig == prev {
			continue
		}
		p.Refresh()
		log.Printf("accounts changed: pool now has %d account(s)", len(store.IDs()))
		prev = sig
	}
}
