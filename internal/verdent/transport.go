package verdent

import (
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// newFingerprintClient 构造伪装成 Chrome 桌面端完整指纹的 tls-client HttpClient：
// TLS JA3 + HTTP/2 Akamai 指纹 + ALPN h2 + 随机 TLS 扩展顺序，彻底对齐桌面端传输层指纹。
// Chrome_133 对应 2025 年初 Chrome，与 Verdent 桌面版（Electron）内核版本接近。
// HTTP 头顺序由 Chrome H2 profile 自动处理（走 H2 时由 HPACK + profile 定序）。
func newFingerprintClient(timeout time.Duration) tls_client.HttpClient {
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithTimeoutSeconds(int(timeout.Seconds())),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithRandomTLSExtensionOrder(),
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		// 构造失败仅因参数非法，运行时不会发生；退回默认 profile。
		c, _ = tls_client.NewHttpClient(tls_client.NewNoopLogger())
		return c
	}
	return c
}

// NewFingerprintClient 导出版，用于登录/刷新等非流式请求，共享同一 Chrome 指纹。
func NewFingerprintClient(timeout time.Duration) tls_client.HttpClient {
	return newFingerprintClient(timeout)
}
