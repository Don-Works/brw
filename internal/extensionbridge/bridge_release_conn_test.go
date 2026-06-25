package extensionbridge

import (
	"context"
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
