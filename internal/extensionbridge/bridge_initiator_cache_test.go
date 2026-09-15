package extensionbridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestInitiatorSensitiveResponsesAreNotCacheable covers the other half of a
// response whose body depends on who asked.
//
// /status carries the handshake token only for a caller tokenServable accepts,
// and /consent is served only to that same caller, so both bodies vary on
// Origin and Sec-Fetch-Site. A 200 that carries a token and says neither
// "do not store this" nor "this varies" is cacheable and indistinguishable from
// the tokenless one — the shape the initiator check exists to close.
func TestInitiatorSensitiveResponsesAreNotCacheable(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		serve  func(*Bridge, http.ResponseWriter, *http.Request)
		// extension is whether the request presents the accepted initiator, so
		// both the served and the refused answer are covered: a refusal cached
		// against the extension would lock it out.
		extension bool
	}{
		{name: "status for the extension", method: http.MethodGet, path: "/status", extension: true,
			serve: func(b *Bridge, w http.ResponseWriter, r *http.Request) { b.handleStatus(w, r) }},
		{name: "status for a page on another site", method: http.MethodGet, path: "/status",
			serve: func(b *Bridge, w http.ResponseWriter, r *http.Request) { b.handleStatus(w, r) }},
		{name: "consent for the extension", method: http.MethodGet, path: "/consent", extension: true,
			serve: func(b *Bridge, w http.ResponseWriter, r *http.Request) { b.handleConsent(w, r) }},
		{name: "consent for a page on another site", method: http.MethodGet, path: "/consent",
			serve: func(b *Bridge, w http.ResponseWriter, r *http.Request) { b.handleConsent(w, r) }},
		{name: "consent revoke for the extension", method: http.MethodPost, path: "/consent/revoke", extension: true,
			serve: func(b *Bridge, w http.ResponseWriter, r *http.Request) { b.handleConsentRevoke(w, r) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, configuredExtnID)
			b.SetAuthToken(tokenIssueSecret)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"all":true}`))
			req.Host = tokenIssueLoopback
			if tc.extension {
				req.Header.Set("Sec-Fetch-Site", initiatorSecFetchSite)
				req.Header.Set("Origin", "chrome-extension://"+configuredExtnID)
			} else {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			rec := httptest.NewRecorder()
			tc.serve(b, rec, req)

			if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
				t.Fatalf("Cache-Control = %q, want no-store on a response whose body depends on the caller", got)
			}
			vary := rec.Header().Values("Vary")
			for _, want := range []string{"Origin", "Sec-Fetch-Site"} {
				found := false
				for _, value := range vary {
					if strings.EqualFold(strings.TrimSpace(value), want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("Vary = %v, want it to name %s, which selects this body", vary, want)
				}
			}
		})
	}
}
