package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// hit drives one request through the full server handler (including the
// host/origin guard) and returns the status code.
func hit(t *testing.T, addr, host, origin string) int {
	t.Helper()
	srv := New(addr, &fakeController{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, req)
	return rec.Code
}

// TestHostGuardLoopbackBind: a loopback bind enforces the Host allowlist
// (DNS-rebinding defense) and rejects cross-origin browser requests (CSRF).
func TestHostGuardLoopbackBind(t *testing.T) {
	const addr = "127.0.0.1:17310"
	cases := []struct {
		name, host, origin string
		want               int
	}{
		{"loopback host, no origin", "127.0.0.1:17310", "", http.StatusOK},
		{"localhost host", "localhost:17310", "", http.StatusOK},
		{"ipv6 loopback host", "[::1]:17310", "", http.StatusOK},
		{"rebinding host rejected", "evil.com:17310", "", http.StatusForbidden},
		{"rebinding bare host rejected", "attacker.test", "", http.StatusForbidden},
		{"cross-origin web page rejected", "127.0.0.1:17310", "https://evil.com", http.StatusForbidden},
		{"loopback origin allowed", "127.0.0.1:17310", "http://127.0.0.1:5173", http.StatusOK},
		{"same-host origin allowed", "127.0.0.1:17310", "http://127.0.0.1:17310", http.StatusOK},
		{"opaque null origin rejected", "127.0.0.1:17310", "null", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hit(t, addr, tc.host, tc.origin); got != tc.want {
				t.Errorf("%s: got status %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestHostGuardTailscaleBind: a non-loopback (Tailscale/LAN) bind does NOT gate
// the Host header — a MagicDNS name or any address reaches the daemon — but the
// cross-origin CSRF guard still applies.
func TestHostGuardTailscaleBind(t *testing.T) {
	const addr = "100.64.0.1:17310" // a Tailscale-style CGNAT address
	cases := []struct {
		name, host, origin string
		want               int
	}{
		{"magicdns host accepted", "my-box.tail1234.ts.net:17310", "", http.StatusOK},
		{"tailscale ip host accepted", "100.64.0.1:17310", "", http.StatusOK},
		{"arbitrary host accepted", "anything.example:17310", "", http.StatusOK},
		{"same-host browser ui allowed", "my-box.tail1234.ts.net:17310", "http://my-box.tail1234.ts.net:8080", http.StatusOK},
		{"cross-origin still rejected", "my-box.tail1234.ts.net:17310", "https://evil.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hit(t, addr, tc.host, tc.origin); got != tc.want {
				t.Errorf("%s: got status %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestHostGuardWildcardBind: a wildcard bind (":port") does not gate Host (the
// operator opened it up) but keeps the CSRF guard.
func TestHostGuardWildcardBind(t *testing.T) {
	if got := hit(t, ":17310", "whatever.example", ""); got != http.StatusOK {
		t.Errorf("wildcard bind should not gate Host, got %d", got)
	}
	if got := hit(t, ":17310", "whatever.example", "https://evil.com"); got != http.StatusForbidden {
		t.Errorf("wildcard bind must still reject cross-origin, got %d", got)
	}
}
