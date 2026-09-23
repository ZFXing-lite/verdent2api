package verdent

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// TestChromeFingerprintHasGREASE 证明 tls-client Chrome 指纹产生的 ClientHello 含 GREASE
// cipher，Go 标准库不含，故断言通过即 TLS 指纹已对齐 Chrome 而非暴露 Go 特征。
func TestChromeFingerprintHasGREASE(t *testing.T) {
	var ciphers []uint16
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// listener 持有 srv.TLS 指针，注入抓取，新握手会调用 GetConfigForClient。
	srv.TLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		ciphers = chi.CipherSuites
		return nil, nil
	}

	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithInsecureSkipVerify(),
		tls_client.WithTimeoutSeconds(10),
		tls_client.WithRandomTLSExtensionOrder(),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, err := fhttp.NewRequest(fhttp.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if len(ciphers) == 0 {
		t.Fatal("no ClientHello captured")
	}
	isGREASE := func(c uint16) bool { return c>>8 == c&0xff && (c&0xff)&0x0f == 0x0a }
	var hasGREASE bool
	for _, cc := range ciphers {
		if isGREASE(cc) { // GREASE 值 0x?a?a，Chrome ClientHello 含之，Go 标准库不含
			hasGREASE = true
		}
	}
	if !hasGREASE {
		t.Errorf("Chrome JA3 should include a GREASE cipher; got %x", ciphers)
	}
}
