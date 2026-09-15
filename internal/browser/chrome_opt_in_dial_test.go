package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/chromeoptin"
)

// The endpoint discovery checks has to be the endpoint brw dials.
//
// chromedp's remote allocator re-fetches /json/version at connect time and
// dials whatever webSocketDebuggerUrl comes back, with no scheme, host or port
// check of its own. A listener holding a port a dead Chrome left behind in
// DevToolsActivePort can therefore answer brw's probe with a loopback address —
// passing validateBrowserWS — and answer the allocator's probe with an address
// anywhere, taking over the whole CDP session. The same answer with the key
// missing entirely hits an unchecked type assertion inside chromedp.
//
// The fixture is that listener: the first /json/version answer is honest and
// every later one is not.
func TestChromeOptInDialsTheEndpointDiscoveryChecked(t *testing.T) {
	elsewhereHits := &atomic.Int64{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	elsewhereWS := "ws://" + hostPort(t, elsewhere.URL) + "/devtools/browser/elsewhere"

	for _, tc := range []struct {
		name string
		// later is what /json/version answers after discovery's own probe.
		later map[string]any
	}{
		{
			name:  "a second answer naming another listener",
			later: map[string]any{"Browser": "Chrome/153.0.0.0", "webSocketDebuggerUrl": elsewhereWS},
		},
		{
			// chromedp asserts the key's type without checking it is there.
			name:  "a second answer with no browser target at all",
			later: map[string]any{"Browser": "Chrome/153.0.0.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := elsewhereHits.Load()
			versionHits := &atomic.Int64{}
			dialedHere := &atomic.Int64{}

			mux := http.NewServeMux()
			var honestWS string
			mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
				body := tc.later
				if versionHits.Add(1) == 1 {
					body = map[string]any{"Browser": "Chrome/153.0.0.0", "webSocketDebuggerUrl": honestWS}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			})
			mux.HandleFunc("/devtools/browser/honest", func(w http.ResponseWriter, r *http.Request) {
				dialedHere.Add(1)
				// Not a real WebSocket upgrade: the connection attempt is the
				// whole observation, and brw failing to attach afterwards is
				// expected.
				w.WriteHeader(http.StatusBadRequest)
			})
			endpointServer := httptest.NewServer(mux)
			defer endpointServer.Close()
			honestWS = "ws://" + hostPort(t, endpointServer.URL) + "/devtools/browser/honest"

			userDataDir := t.TempDir()
			writeActivePortFile(t, userDataDir, endpointServer.URL, "/devtools/browser/honest")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			endpoint, err := chromeoptin.Discover(ctx, chromeoptin.Options{UserDataDir: userDataDir})
			if err != nil {
				t.Fatalf("discovery refused an endpoint that answered honestly: %v", err)
			}
			if endpoint.BrowserWSURL != honestWS {
				t.Fatalf("discovered %q, want %q", endpoint.BrowserWSURL, honestWS)
			}

			// The attach fails: the fixture speaks no CDP. What matters is where
			// it was attempted.
			attachCtx, attachCancel := context.WithTimeout(ctx, 10*time.Second)
			defer attachCancel()
			manager, err := New(attachCtx, Config{
				RemoteURL:       endpoint.HTTPURL,
				BrowserWSURL:    endpoint.BrowserWSURL,
				AttachOnly:      true,
				SignedInProfile: true,
				Timeout:         5 * time.Second,
			})
			if err == nil {
				_ = manager.Close()
				t.Fatal("attaching to a fixture that speaks no CDP succeeded")
			}

			if got := elsewhereHits.Load() - before; got != 0 {
				t.Fatalf("brw made %d request(s) to the listener the second answer named; the dialled URL must be the one discovery checked", got)
			}
			if got := versionHits.Load(); got != 1 {
				t.Fatalf("/json/version was fetched %d times; only discovery may ask, or the check applies to a URL nobody dials", got)
			}
			if got := dialedHere.Load(); got == 0 {
				t.Fatal("brw never dialled the browser target discovery checked")
			}
		})
	}
}

func hostPort(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.Host
}

// writeActivePortFile records a port the way Chrome does: the port on the first
// line and the browser target's path on the second.
func writeActivePortFile(t *testing.T, userDataDir, serverURL, wsPath string) {
	t.Helper()
	_, port, found := strings.Cut(hostPort(t, serverURL), ":")
	if !found {
		t.Fatalf("no port in %q", serverURL)
	}
	contents := fmt.Sprintf("%s\n%s\n", port, wsPath)
	if err := os.WriteFile(filepath.Join(userDataDir, "DevToolsActivePort"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write DevToolsActivePort: %v", err)
	}
}
