// cmd/server 是 verdent2api 的主网关服务。
//
// 把 api.verdent.ai（Verdent 官方 OpenAI 兼容网关）的多个 API key
// 封装成一个 OpenAI 兼容端点，提供账号池轮换 / 冷却状态机 / 流式透传。
//
//	./verdent-server -config config.json
//	GET  /v1/models
//	POST /v1/chat/completions   （支持 stream）
//	GET  /status                账号池快照
//	GET  /healthz
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

	"verdent2api/internal/auth"
	"verdent2api/internal/pool"
	"verdent2api/internal/server"
	"verdent2api/internal/upstream"
)

const version = "1.0.0"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := auth.New(cfg.KeyFile)
	if err != nil {
		log.Fatalf("auth store: %v", err)
	}
	if keys := store.Keys(); len(keys) == 0 {
		log.Printf("warning: no API key found in %s; run the login tool or add keys manually", cfg.KeyFile)
		log.Printf("         then POST /v1/chat/completions will return 503 until at least one key is healthy")
	}

	client := upstream.New(
		cfg.Upstream.BaseURL,
		cfg.Upstream.TimeoutSeconds*time.Second,
		cfg.Upstream.HeaderTimeoutSeconds*time.Second,
		cfg.Upstream.IdleTimeoutSeconds*time.Second,
		cfg.Upstream.UserAgent,
	)

	p := pool.New(
		pool.Config{
			MaxInFlight:    cfg.Pool.MaxInFlight,
			ErrThreshold:   cfg.Pool.ErrThreshold,
			ErrCooldown:    cfg.Pool.ErrCooldown,
			RateCooldown:   cfg.Pool.RateCooldown,
			CreditCooldown: cfg.Pool.CreditCooldown,
			SelectJitterMS: cfg.Pool.SelectJitterMS,
		},
		client,
		store,
		cfg.StateFile,
	)

	srv := &server.Server{
		Pool:           p,
		Client:         client,
		APIKey:         cfg.APIKey,
		ModelMap:       cfg.ModelMap,
		ModelAlias:     cfg.ModelAlias,
		StripReasoning: cfg.Features.StripReasoning,
		Version:        version,
	}

	mux := http.NewServeMux()
	srv.Register(mux)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 60 * time.Second,
		ReadTimeout:       0, // 流式请求不限制读取时长
		IdleTimeout:       120 * time.Second,
	}

	// 后台保活：定期探活 key 并落盘状态。
	if cfg.Scheduler.Enabled {
		go runKeeper(cfg, client, p)
	}

	// key 文件热重载：登录工具写入新 key 后无需重启服务，
	// 定期重读凭证文件，变化时同步进池（15s 粒度，登录后近实时可用）。
	go runKeyReloader(store, p)

	go func() {
		log.Printf("verdent2api %s listening on http://127.0.0.1%s (upstream %s)",
			version, cfg.Listen, client.BaseURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// runKeeper 定期保活：拉 /v1/models 验证 key 可用性，
// 顺便把冷却到期的 key 自动放回（时间到了自然生效，这里只负责探活记数）。
func runKeeper(cfg *Config, client *upstream.Client, p *pool.Pool) {
	interval := time.Duration(cfg.Scheduler.KeepaliveMinutes) * time.Minute
	if interval < time.Minute {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		for _, st := range p.Snapshot() {
			if st.Disabled {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := client.Models(ctx, st.APIKey)
			cancel()
			if err != nil {
				// 探活失败只记录，不触发冷却（真实请求会做分类）。
				log.Printf("keepalive probe failed for %s: %v", maskKey(st.APIKey), err)
			}
		}
	}
}

// runKeyReloader 周期性重读凭证文件；key 集合变化时刷新账号池。
// 让 login 工具新建的 key 无需重启 server 即可生效。
func runKeyReloader(store *auth.Store, p *pool.Pool) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	prev := keySignature(store.Keys())
	for range ticker.C {
		if err := store.Reload(); err != nil {
			log.Printf("key reload failed: %v", err)
			continue
		}
		sig := keySignature(store.Keys())
		if sig == prev {
			continue
		}
		p.Refresh()
		log.Printf("key file changed: pool now has %d key(s)", len(store.Keys()))
		prev = sig
	}
}

// keySignature 把 key 集合折叠成一个可比较签名（顺序无关，keys 已排序）。
func keySignature(keys []string) string {
	return strings.Join(keys, "\x00")
}

func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}

// loggingMiddleware 记录访问日志（方法/路径/状态/耗时）。
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)",
			r.Method, strings.TrimRight(r.URL.Path, "/"), rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
