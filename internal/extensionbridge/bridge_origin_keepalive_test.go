package extensionbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/coder/websocket"
)

// TestEffectiveExtensionID locks the origin-resolution logic. NOTE: the
// origin-hardening is WIRED but currently INERT — profilepolicy.
// DefaultBridgeExtensionID is "" (no published id baked into the repo yet), so an
// unconfigured bridge still falls back to the chrome-extension://* wildcard with
// a loud warning. The hardening activates automatically the moment that const is
// populated with the real published id. The valuable, active part of this change
// is the ping keepalive (tested below), not the origin default.
func TestEffectiveExtensionID(t *testing.T) {
	// Explicitly configured: the profile id always wins (the meaningful guarantee).
	explicit := New("", time.Second, "abcdefghijklmnopabcdefghijklmnop")
	if got := explicit.effectiveExtensionID(); got != "abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("configured effectiveExtensionID = %q, want the profile id", got)
	}

	// Unconfigured: mirrors the published default const (guards against anyone
	// reintroducing a wildcard/hard-coded literal here). Today that is "", which
	// the wildcard-fallback path in handleExtension keys off.
	unset := New("", time.Second, "")
	if got, want := unset.effectiveExtensionID(), strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID); got != want {
		t.Fatalf("unconfigured effectiveExtensionID = %q, want default %q", got, want)
	}
	if profilepolicy.DefaultBridgeExtensionID == "" && unset.effectiveExtensionID() != "" {
		t.Fatal("with no published default, the effective id must be empty (wildcard fallback)")
	}
}

// TestConfiguredExtensionOriginAcceptedAndOthersRejected proves the real
// extension origin is still accepted when an id is configured, while a different
// extension origin is rejected by the websocket Origin check.
func TestConfiguredExtensionOriginAcceptedAndOthersRejected(t *testing.T) {
	const id = "abcdefghijklmnopabcdefghijklmnop"
	b := New("", time.Second, id)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	// The configured extension's origin must connect.
	okCtx, okCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer okCancel()
	conn, _, err := websocket.Dial(okCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://" + id}},
	})
	if err != nil {
		t.Fatalf("configured extension origin must be accepted: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	// A different extension's origin must be rejected.
	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	bad, _, err := websocket.Dial(badCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}},
	})
	if err == nil {
		_ = bad.Close(websocket.StatusNormalClosure, "should not have connected")
		t.Fatal("a non-configured extension origin must be rejected")
	}
}

// TestKeepAliveStopsWhenConnCloses proves the pinger exits cleanly when the
// connection closes / the context is cancelled — no goroutine leak. It runs
// keepAlive directly so the test does not have to wait the 30s production
// interval, then asserts the goroutine returns promptly after cancellation.
func TestKeepAliveStopsWhenConnCloses(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Fast interval so the pinger is actively ticking when we cancel.
		b.keepAlive(ctx, b.serverConn(), 5*time.Millisecond)
		close(done)
	}()
	// Let it ping a few times against the live conn (pings must succeed).
	time.Sleep(30 * time.Millisecond)

	// Cancel (mimicking readLoop returning) and require the pinger to exit.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepAlive did not exit after context cancel; goroutine leak")
	}
}

// TestKeepAliveClosesConnOnDeadLink proves a ping failure (dead/half-open link)
// closes the conn so b.pending can drain. We simulate a dead link by closing the
// client end; the server-side ping then fails and keepAlive closes the conn and
// returns.
func TestKeepAliveClosesConnOnDeadLink(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serverConn := b.serverConn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		b.keepAlive(ctx, serverConn, 5*time.Millisecond)
		close(done)
	}()

	// Kill the client end abruptly so server-side pings stop being answered.
	_ = conn.CloseNow()

	select {
	case <-done:
		// keepAlive returned: it detected the failure (or the conn read side
		// closing) and exited. Good.
	case <-time.After(3 * time.Second):
		t.Fatal("keepAlive did not exit after the link died")
	}
}

// TestKeepAliveExitsWhenConnReplaced proves the pinger for a superseded
// connection goes quiet once it is no longer the bridge's active conn, even
// before its context is cancelled — guarding against a stale pinger pinging a
// replaced socket. Driven by directly clearing b.conn and using a tiny ticker
// via a short-lived run.
func TestKeepAliveExitsWhenConnReplaced(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	serverConn := b.serverConn()
	// Simulate this conn being replaced/cleared by the bridge.
	b.mu.Lock()
	b.conn = nil
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		// Fast interval: on its first tick the pinger sees b.conn != serverConn
		// and returns WITHOUT us cancelling, exercising the not-current branch.
		b.keepAlive(ctx, serverConn, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replaced-conn keepAlive did not exit on its own when no longer the active conn")
	}
}

// serverConn returns the bridge's currently-registered server-side conn for
// tests.
func (b *Bridge) serverConn() *websocket.Conn {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.conn
}
