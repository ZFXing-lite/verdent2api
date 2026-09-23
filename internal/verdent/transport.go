package verdent

import (
	"context"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// newFingerprintTransport 构造伪装成 Chrome 桌面端 TLS 指纹(JA3)的 Transport。
//
// go1.19 限制:标准库 http2 升级要求 *tls.Conn,而 utls 的 UConn 不是该类型,
// 无法做 H2-over-uTLS。故 ALPN 仅协商 http/1.1,走 HTTP/1.1 over Chrome
// ClientHello。JA3 与 Chrome 一致(ALPN 扩展存在,值不参与 JA3 hash);
// JA4 的 ALPN 字段为 http/1.1 而非 h2,属残留次级指纹,远好于裸 Go 标准库。
func newFingerprintTransport(idleTimeout, headerTimeout time.Duration) *http.Transport {
	dialTLS := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		raw, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		// NextProtos=["http/1.1"]:让服务器只选 http/1.1,避免协商出 h2 后
		// 标准库无法驱动 utls 连接走 H2。ALPN 扩展仍存在,JA3 不受影响。
		cfg := &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}
		uConn := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
		if err := uConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return uConn, nil
	}
	return &http.Transport{
		DialTLSContext:        dialTLS,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       idleTimeout,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     false, // utls 连接非 *tls.Conn,标准库无法升级 H2,显式走 H1.1
	}
}

// NewFingerprintClient 返回带 Chrome TLS 指纹的 HTTP 客户端,用于登录/刷新等
// 非流式请求,使其 TLS 指纹与桌面端一致而非暴露 Go 标准库特征。
func NewFingerprintClient(timeout time.Duration) *http.Client {
	tr := newFingerprintTransport(120*time.Second, 120*time.Second)
	return &http.Client{Transport: tr, Timeout: timeout}
}
