package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestStaleConnTeardownPreservesLiveState proves the connection-replacement
// fix: when a displaced (stale) connection's readLoop returns, its teardown
// must NOT drain pending RPCs that belong to the live connection, must NOT
// clear b.conn, and must NOT stamp a disconnect reason while a healthy socket
// is still active. Only the active connection's own teardown drains.
//
// MV3 service workers reconnect constantly and handleExtension replaces the old
// conn with the new one, so the old conn's readLoop returning after the swap is
// a NORMAL occurrence, not an error path.
func TestStaleConnTeardownPreservesLiveState(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
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
	live := b.serverConn()

	// Register a pending RPC that belongs to the live connection.
	ch := make(chan response, 1)
	b.mu.Lock()
	b.pending["test-999"] = ch
	b.mu.Unlock()

	// A displaced/stale connection's readLoop returns. Its conn pointer is not
	// the active one (nil here stands in for "any conn that is not b.conn").
	// With the guard this must be a no-op against live state.
	b.releaseConn(nil, "stale closed")

	b.mu.Lock()
	_, stillPending := b.pending["test-999"]
	stillConnected := b.conn == live
	reason := b.disconnectReason
	b.mu.Unlock()
	if !stillPending {
		t.Fatal("stale-conn teardown drained the LIVE connection's pending RPC")
	}
	if !stillConnected {
		t.Fatal("stale-conn teardown cleared the live b.conn")
	}
	if reason != "" {
		t.Fatalf("stale-conn teardown stamped disconnect reason %q while still connected", reason)
	}

	// The live connection's own teardown MUST drain its pending RPCs.
	b.releaseConn(live, "closed")
	select {
	case r := <-ch:
		if r.Error == "" {
			t.Fatal("drained pending RPC should carry a disconnect error")
		}
	default:
		t.Fatal("live-conn teardown did not drain the pending RPC")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.conn != nil {
		t.Fatal("live-conn teardown left b.conn non-nil")
	}
	if b.disconnectReason != "closed" {
		t.Fatalf("disconnectReason = %q, want \"closed\"", b.disconnectReason)
	}
}

// sendIdentityHello sends a full hello frame carrying a configured identity, so
// replace-time identity comparison has something to compare.
func sendIdentityHello(t *testing.T, conn *websocket.Conn, token, workspace, profile, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, _ := json.Marshal(map[string]any{
		"type": "hello",
		"hello": map[string]any{
			"source":    "brw-extension",
			"token":     token,
			"workspace": workspace,
			"profile":   profile,
			"label":     label,
		},
	})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("write hello: %v", err)
	}
}

// TestReplaceByDifferentExtensionDrainsPending proves the two sides of the
// replace-time pending contract:
//   - a replacement by a DIFFERENT extension identity (another browser profile
//     colliding onto this bridge) fails in-flight RPCs immediately with
//     replacedDrainReason — the displaced extension can never answer them, and
//     without the drain each call would hang for its full timeout;
//   - a replacement by the SAME identity (an MV3 service worker reconnecting
//     mid-call) preserves pending, because the same worker can still answer
//     over the new socket.
func TestReplaceByDifferentExtensionDrainsPending(t *testing.T) {
	const token = "replace-test-token"
	b := New("", time.Second, "")
	b.SetAuthToken(token)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	// CloseNow throughout: displaced conns are already dead server-side, and a
	// graceful Close would park ~5s each waiting for a close frame that never comes.
	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.CloseNow()
	sendIdentityHello(t, connA, token, "w1", "p1", "browser A")
	waitUntil(t, b.liveConn)

	chDrained := make(chan response, 1)
	b.mu.Lock()
	b.pending["replace-1"] = chDrained
	b.mu.Unlock()

	// A DIFFERENT identity takes over: pending must drain with replacedDrainReason.
	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()
	sendIdentityHello(t, connB, token, "w2", "p2", "browser B")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.hello.Workspace == "w2"
	})
	select {
	case r := <-chDrained:
		if r.Error != replacedDrainReason {
			t.Fatalf("drained RPC error = %q, want %q", r.Error, replacedDrainReason)
		}
	default:
		t.Fatal("identity-changing replace did not drain the in-flight RPC")
	}

	liveB := b.serverConn()
	chKept := make(chan response, 1)
	b.mu.Lock()
	b.pending["replace-2"] = chKept
	b.mu.Unlock()

	// The SAME identity reconnects (MV3 worker churn): pending must survive.
	connC, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial C: %v", err)
	}
	defer connC.CloseNow()
	sendIdentityHello(t, connC, token, "w2", "p2", "browser B")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.conn != liveB
	})
	select {
	case r := <-chKept:
		t.Fatalf("same-identity replace drained the in-flight RPC (error %q)", r.Error)
	default:
		// still pending — same worker may answer on the new socket
	}
	b.mu.RLock()
	_, stillPending := b.pending["replace-2"]
	b.mu.RUnlock()
	if !stillPending {
		t.Fatal("same-identity replace removed the pending RPC")
	}
}
