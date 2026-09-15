package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

// isSnapshotWalkExpression reports whether an expression the bridge sent is a
// snapshot walk. Both forms carry the walker's private, per-process property
// name: the install-and-call form assigns it, the call-only form invokes it. The
// fakes in this package classify on this rather than on either literal shape, so
// a fake cannot go on recognising one form and silently answering the other with
// something that is not a snapshot.
func isSnapshotWalkExpression(expression string) bool {
	return strings.Contains(expression, "__brw_snap_")
}

// walkerExtension is a fake service worker that models the ONE thing this test is
// about: a page where the DOM walker is installed by the first snapshot and is
// still there for the next one. It records every expression the bridge asks it to
// evaluate, so the test can see what actually crosses the websocket.
type walkerExtension struct {
	mu          sync.Mutex
	expressions []string
	installed   bool
	cold        string
}

func (w *walkerExtension) recorded() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.expressions...)
}

func (w *walkerExtension) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expressions = nil
}

func (w *walkerExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		reply := map[string]any{"id": msg.ID, "ok": true, "result": map[string]any{}}
		switch msg.Type {
		case "get_active_tab_id":
			reply["result"] = map[string]any{"tabId": 12}
		case "list_tabs":
			reply["result"] = []map[string]any{{
				"id": 12, "url": "https://fixture.test/", "title": "fixture",
				"active": true, "windowId": 1, "windowFocused": true, "windowType": "normal",
			}}
		case "cdp":
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			expression, _ := params["expression"].(string)
			if method != "Runtime.evaluate" || expression == "" {
				break
			}
			w.mu.Lock()
			w.expressions = append(w.expressions, expression)
			isCold := expression == w.cold
			installed := w.installed
			if isCold {
				w.installed = true
			}
			w.mu.Unlock()
			if !isCold && !installed {
				// The page has no walker yet, so the call expression throws exactly as
				// it would in Chrome. This is what drives the bridge's cold fallback.
				reply["ok"] = false
				reply["error"] = "TypeError: window.__brw_snap is not a function"
				break
			}
			reply["result"] = map[string]any{"result": map[string]any{"value": map[string]any{
				"url":      "https://fixture.test/",
				"title":    "fixture",
				"elements": []map[string]any{{"ref": "e1", "role": "button", "name": "Buy", "tag": "button"}},
				"metadata": map[string]any{"version": 1},
			}}}
		}
		out, _ := json.Marshal(reply)
		_ = conn.Write(ctx, websocket.MessageText, out)
	}
}

func connectWalkerExtension(t *testing.T, b *Bridge, w *walkerExtension) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	// A browser WebSocket has no read cap; coder/websocket defaults to 32KiB, which
	// is smaller than the walker install expression this test exists to measure.
	conn.SetReadLimit(8 << 20)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go w.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// TestBridgeSnapshotShipsTheWalkerOncePerDocument is the byte half of the
// extension-bridge snapshot change. The bridge used to format
// snapshot.SnapshotFunctionScript into every snapshot request, so the whole
// walker source crossed the websocket on EVERY snapshot of the same page. It now
// ships the walker once and calls it by name afterwards.
//
// The test reads the expressions off the wire rather than trusting the call site,
// and asserts the second snapshot of the same document carries the SHORT one.
func TestBridgeSnapshotShipsTheWalkerOncePerDocument(t *testing.T) {
	opts := snapshot.SnapshotOptions{Mode: "all"}
	hot, cold := snapshot.SnapshotCallExpressions(opts)

	b := New("", 5*time.Second, "")
	ext := &walkerExtension{cold: cold}
	cleanup := connectWalkerExtension(t, b, ext)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := b.Snapshot(ctx, opts); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	first := ext.recorded()
	if len(first) != 2 || first[0] != hot || first[1] != cold {
		t.Fatalf("first snapshot should try the call and fall back to the install; got %d expressions (match hot=%v cold=%v)",
			len(first), len(first) > 0 && first[0] == hot, len(first) > 1 && first[1] == cold)
	}

	ext.reset()
	if _, err := b.Snapshot(ctx, opts); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	second := ext.recorded()
	if len(second) != 1 {
		t.Fatalf("second snapshot of the same document sent %d expressions, want 1", len(second))
	}
	if second[0] != hot {
		t.Fatalf("second snapshot did not use the installed walker; sent %d bytes", len(second[0]))
	}

	// Recorded numbers, not a vague "fewer": the walker source is what used to go
	// out every time.
	t.Logf("snapshot request bytes: before=%d (whole walker), after=%d (call only), saved=%d per repeat snapshot",
		len(cold), len(hot), len(cold)-len(hot))
	if len(hot) >= len(cold)/10 {
		t.Fatalf("the repeat-snapshot expression is %d bytes against a %d-byte install; expected an order of magnitude less", len(hot), len(cold))
	}
}

// TestBridgeSnapshotRunsTheSameWalkerAsDirectCDP is the ref half. The two
// transports have to mint the same refs for the same page, and the only way that
// holds through future edits is for both to run one walker source. The bridge
// relays an expression the extension does not interpret, so "same refs" reduces
// to "same expression", which is what this asserts — against the snapshot
// package's own builder, for several option shapes.
func TestBridgeSnapshotRunsTheSameWalkerAsDirectCDP(t *testing.T) {
	cases := []snapshot.SnapshotOptions{
		{Mode: "all"},
		{Mode: "frontier", Limit: 40, ViewportOnly: true},
		{Mode: "all", Query: "checkout", TextContent: true},
	}
	for _, opts := range cases {
		hot, cold := snapshot.SnapshotCallExpressions(opts)
		if !strings.Contains(cold, snapshot.SnapshotFunctionScript) {
			t.Fatalf("the install expression for %+v does not carry the shared walker source", opts)
		}
		if strings.Contains(hot, "function(opts)") {
			t.Fatalf("the call expression for %+v carries walker source; it should only call the installed one", opts)
		}
		// Both halves must name the same installed function, or a bridge snapshot
		// and a direct-CDP snapshot would be two different walkers on one page.
		name := hot[:strings.Index(hot, "(")]
		if !strings.Contains(cold, name+"=") {
			t.Fatalf("call expression %q and install expression do not agree on the walker name", name)
		}
	}
}
