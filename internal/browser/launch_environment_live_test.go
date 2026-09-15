package browser

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// launchedManager starts a real headless Chrome through the same launcher the
// daemon uses, so the switches under test are the ones a user would get. The
// per-tab overrides can be tested against a shared browser; these cannot —
// Chrome reads a proxy and a certificate policy once, at startup.
func launchedManager(t *testing.T, network cdplaunch.NetworkEnvironment) *Manager {
	t.Helper()
	if _, err := cdplaunch.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	profile := browsertest.NewProfile(t)
	m, err := New(context.Background(), Config{
		UserDataDir: profile.Dir(),
		Headless:    true,
		Timeout:     20 * time.Second,
		Network:     network,
	})
	if err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	// Close only waits for Chrome's root process, which on Linux exits while a
	// helper is still writing the profile; the reclaim holds the directory gone
	// before testing's own removal runs.
	profile.StopWith(func() { _ = m.Close() })
	return m
}

const proxiedBody = "served-by-the-fixture-proxy"

// A proxy is only configured if the browser's traffic actually goes through it.
func TestProxyServerSendsTrafficThroughTheProxy(t *testing.T) {
	var mu sync.Mutex
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A proxied request carries the absolute URL in the request line, which is
		// how this handler can tell it is acting as a proxy rather than an origin.
		mu.Lock()
		proxied = append(proxied, r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p id="content">%s</p></body></html>`, proxiedBody)
	}))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	m := launchedManager(t, cdplaunch.NetworkEnvironment{
		ProxyServer: proxyURL.Host,
		// Loopback goes direct anyway; naming it proves the switch is accepted
		// alongside the proxy rather than rejected as a pair.
		ProxyBypassList: "<local>",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const target = "http://brw-proxy-fixture.example/hello"
	if _, err := m.Open(ctx, target); err != nil {
		t.Fatalf("open through the proxy: %v", err)
	}

	content := evaluateString(t, m, ctx, `document.getElementById("content") ? document.getElementById("content").textContent : "no-content"`)
	if content != proxiedBody {
		t.Fatalf("page content = %q, want %q — the request did not come back through the proxy", content, proxiedBody)
	}
	mu.Lock()
	seen := append([]string(nil), proxied...)
	mu.Unlock()
	var sawTarget bool
	for _, requested := range seen {
		if strings.Contains(requested, "brw-proxy-fixture.example") {
			sawTarget = true
		}
	}
	if !sawTarget {
		t.Fatalf("the proxy never saw a request for the target host; it saw %q", seen)
	}
}

const tlsFixtureBody = "served-over-a-private-certificate"

// Chrome refuses a certificate it cannot build a chain for. The two ways past it
// differ in blast radius, and the narrow one has to actually work or nobody will
// use it over the broad one.
func TestCertificatePolicyDecidesWhetherAPrivateCertificateLoads(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p id="content">%s</p></body></html>`, tlsFixtureBody)
	}))
	defer srv.Close()

	trusted, err := cdplaunch.SPKIFingerprintsFromPEM(certificatePEM(t, srv))
	if err != nil {
		t.Fatalf("SPKIFingerprintsFromPEM: %v", err)
	}

	tests := []struct {
		name     string
		network  cdplaunch.NetworkEnvironment
		wantLoad bool
	}{
		{
			name:     "default refuses an untrusted certificate",
			network:  cdplaunch.NetworkEnvironment{},
			wantLoad: false,
		},
		{
			name:     "ignore-https-errors accepts anything",
			network:  cdplaunch.NetworkEnvironment{IgnoreHTTPSErrors: true},
			wantLoad: true,
		},
		{
			name:     "naming the key accepts only that certificate",
			network:  cdplaunch.NetworkEnvironment{TrustedSPKI: trusted},
			wantLoad: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := launchedManager(t, tt.network)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// A refused certificate shows an interstitial instead of the page, and
			// may also surface as a navigation error; both are "did not load".
			_, openErr := m.Open(ctx, srv.URL)
			content := ""
			if openErr == nil {
				content = evaluateString(t, m, ctx, `document.getElementById("content") ? document.getElementById("content").textContent : ""`)
			}
			loaded := content == tlsFixtureBody
			if loaded != tt.wantLoad {
				t.Fatalf("page loaded = %v (content %q, open error %v), want loaded = %v", loaded, content, openErr, tt.wantLoad)
			}
		})
	}
}

func certificatePEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("the TLS fixture server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}
