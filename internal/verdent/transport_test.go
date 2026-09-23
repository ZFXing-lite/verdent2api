package verdent

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TestChromeFingerprintHasGREASE 证明 uTLS Chrome 指纹产生的 ClientHello 含 GREASE
// cipher 0x0a0a。Go 标准库 crypto/tls 不发 GREASE，故该断言通过即证明 TLS 指纹
// 已从 Go 默认切到 Chrome，不再暴露 Go 客户端特征。
func TestChromeFingerprintHasGREASE(t *testing.T) {
	var ciphers []uint16
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// listener 持有 srv.TLS 指针，在其上注入抓取，新握手会调用 GetConfigForClient。
	srv.TLS.NextProtos = []string{"http/1.1"}
	srv.TLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		ciphers = chi.CipherSuites
		return nil, nil
	}

	addr := srv.Listener.Addr().String()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	host, _, _ := net.SplitHostPort(addr)
	cfg := &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true}
	uConn := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
	if err := uConn.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("utls handshake: %v", err)
	}
	_ = uConn.Close()

	if len(ciphers) == 0 {
		t.Fatal("no ClientHello captured")
	}
	isGREASE := func(c uint16) bool { return c>>8 == c&0xff && (c&0xff)&0x0f == 0x0a }
	var hasGREASE bool
	for _, c := range ciphers {
		if isGREASE(c) { // GREASE 值 0x?a?a，Chrome ClientHello 含之，Go 标准库不含
			hasGREASE = true
		}
	}
	if !hasGREASE {
		t.Errorf("Chrome JA3 should include a GREASE cipher; got %x", ciphers)
	}
}
