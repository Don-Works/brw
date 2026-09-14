package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func TestProbeReachableReportsIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"identity":{"workspace":"work","profile":"work-profile","mode":"bridge"}}`))
	}))
	defer srv.Close()

	rec := Probe(profilepolicy.Profile{
		Name:                   "work-profile",
		Kind:                   "chrome",
		ExtensionBridgeAllowed: true,
		BridgeHTTPAddr:         srv.URL, // httptest gives http://127.0.0.1:PORT
		BridgeWSAddr:           "127.0.0.1:19999",
	}, 3*time.Second)

	if !rec.Reachable {
		t.Fatalf("Probe reachable=false, want true (error=%q)", rec.Error)
	}
	if rec.Identity == nil || rec.Identity.Profile != "work-profile" || rec.Identity.Mode != "bridge" {
		t.Fatalf("identity not captured from /health: %+v", rec.Identity)
	}
	if rec.Workspace != "work" {
		t.Fatalf("workspace = %q, want \"work\" (from probed identity)", rec.Workspace)
	}
	if rec.HTTPAddr != srv.URL {
		t.Fatalf("http_addr = %q, want %q", rec.HTTPAddr, srv.URL)
	}
	if rec.ExtensionID != profilepolicy.DefaultBridgeExtensionID {
		t.Fatalf("extension_id = %q, want the default %q", rec.ExtensionID, profilepolicy.DefaultBridgeExtensionID)
	}
}

func TestProbeUnreachableIsRecordedNotFatal(t *testing.T) {
	// Port 1 on loopback has nothing listening: the probe must fail fast and be
	// recorded as unreachable rather than dropping the daemon from the listing.
	rec := Probe(profilepolicy.Profile{
		Name:                   "down-profile",
		ExtensionBridgeAllowed: true,
		BridgeHTTPAddr:         "127.0.0.1:1",
	}, 500*time.Millisecond)

	if rec.Reachable {
		t.Fatal("Probe reachable=true for a dead address, want false")
	}
	if rec.Error == "" {
		t.Fatal("unreachable daemon must record a non-empty error")
	}
	if rec.Name != "down-profile" || rec.HTTPAddr != "http://127.0.0.1:1" {
		t.Fatalf("record fields wrong for unreachable daemon: %+v", rec)
	}
	if rec.Identity != nil {
		t.Fatalf("unreachable daemon must have nil identity, got %+v", rec.Identity)
	}
}

func TestCandidatesDropsProfilesWithoutABridge(t *testing.T) {
	path := writePolicy(t, profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		{Name: "direct-only", DirectCDPAllowed: true},
		{Name: "bridge-a", ExtensionBridgeAllowed: true, BridgeHTTPAddr: "127.0.0.1:17410"},
		{Name: "bridge-b", ExtensionBridgeAllowed: true},
	}})

	profiles, err := Candidates(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Name != "bridge-a" || profiles[1].Name != "bridge-b" {
		t.Fatalf("candidates = %+v, want the two extension-bridge profiles", profiles)
	}
	if got := HTTPURL(profiles[0]); got != "http://127.0.0.1:17410" {
		t.Fatalf("HTTPURL = %q, want the configured address with a scheme", got)
	}
	if got := HTTPURL(profiles[1]); got != "http://127.0.0.1:17310" {
		t.Fatalf("HTTPURL = %q, want the default daemon address", got)
	}
	if got := WSAddr(profiles[1]); got != "127.0.0.1:17311" {
		t.Fatalf("WSAddr = %q, want the default bridge address", got)
	}
}

func writePolicy(t *testing.T, policy profilepolicy.Policy) string {
	t.Helper()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "browser-profiles.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
