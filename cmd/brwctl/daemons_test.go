package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func TestProbeDaemonReachableReportsIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"identity":{"workspace":"work","profile":"work-profile","mode":"bridge"}}`))
	}))
	defer srv.Close()

	rec := probeDaemon(profilepolicy.Profile{
		Name:                   "work-profile",
		Kind:                   "chrome",
		ExtensionBridgeAllowed: true,
		BridgeHTTPAddr:         srv.URL, // httptest gives http://127.0.0.1:PORT
		BridgeWSAddr:           "127.0.0.1:19999",
	}, 3*time.Second)

	if !rec.Reachable {
		t.Fatalf("probeDaemon reachable=false, want true (error=%q)", rec.Error)
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

func TestProbeDaemonUnreachableIsRecordedNotFatal(t *testing.T) {
	// Port 1 on loopback has nothing listening: the probe must fail fast and be
	// recorded as unreachable rather than dropping the daemon from the listing.
	rec := probeDaemon(profilepolicy.Profile{
		Name:                   "down-profile",
		ExtensionBridgeAllowed: true,
		BridgeHTTPAddr:         "127.0.0.1:1",
	}, 500*time.Millisecond)

	if rec.Reachable {
		t.Fatal("probeDaemon reachable=true for a dead address, want false")
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
