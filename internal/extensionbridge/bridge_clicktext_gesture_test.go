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

// clickTextScriptMarker is a string that only the click_text walker contains, so
// the fake extension can tell that evaluation apart from the snapshot and settle
// evaluations that surround it.
const clickTextScriptMarker = "no visible element found for text "

// evalRecorder is a fake extension that answers every bridge command and records
// the Runtime.evaluate parameters it was asked to run.
type evalRecorder struct {
	mu    sync.Mutex
	evals []map[string]any
}

func (r *evalRecorder) record(params map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evals = append(r.evals, params)
}

// clickTextEval returns the recorded Runtime.evaluate that ran the click_text
// walker, and whether one was seen at all.
func (r *evalRecorder) clickTextEval() (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, params := range r.evals {
		if expr, _ := params["expression"].(string); strings.Contains(expr, clickTextScriptMarker) {
			return params, true
		}
	}
	return nil, false
}

func (r *evalRecorder) serve(ctx context.Context, conn *websocket.Conn) {
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
		var result any = map[string]any{}
		switch msg.Type {
		case "list_tabs":
			result = []map[string]any{{"id": 7, "windowId": 1, "active": true, "url": "https://fixture.test/", "title": "Fixture"}}
		case "get_active_tab_id":
			result = map[string]any{"tabId": 7}
		case "cdp":
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			if method == "Runtime.evaluate" {
				r.record(params)
				expr, _ := params["expression"].(string)
				result = map[string]any{"result": map[string]any{"value": evalReply(expr)}}
			}
		}
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

// evalReply answers each in-page evaluation with the shape its caller decodes.
func evalReply(expr string) any {
	switch {
	case strings.Contains(expr, clickTextScriptMarker):
		return map[string]any{"ok": true, "x": 12, "y": 34, "tag": "button", "role": "button", "name": "Add to cart"}
	case strings.Contains(expr, "elements"):
		return map[string]any{"url": "https://fixture.test/", "title": "Fixture", "elements": []any{}}
	default:
		return map[string]any{"ok": true}
	}
}

func connectEvalRecorder(t *testing.T, b *Bridge) (*evalRecorder, func()) {
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
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	// The click_text walker ships as one large expression; the default websocket
	// read limit closes the connection before it arrives.
	conn.SetReadLimit(8 << 20)
	rec := &evalRecorder{}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go rec.serve(serveCtx, conn)
	return rec, func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// The extension transport ran the click_text walker under a plain
// Runtime.evaluate and only re-ran it with userGesture when the walker had
// DEFERRED — which it can only do for handler shapes that are readable from page
// script. A listener registered with addEventListener is not readable, so a
// gesture-gated call inside one was never given the activation it needs and was
// dropped under a click that reported success. The activation has to be on the
// first evaluation, for every target.
func TestBridgeClickTextEvaluatesWithAUserGesture(t *testing.T) {
	b := New("", 5*time.Second, "")
	rec, cleanup := connectEvalRecorder(t, b)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := b.ClickText(ctx, snapshot.ClickTextOptions{Text: "Add to cart"}); err != nil {
		t.Fatalf("click text: %v", err)
	}

	params, ok := rec.clickTextEval()
	if !ok {
		t.Fatal("the bridge never ran the click_text walker")
	}
	if gesture, _ := params["userGesture"].(bool); !gesture {
		t.Fatalf("click_text evaluated with userGesture=%v, want true: without it a gesture-gated "+
			"addEventListener handler runs with no transient activation and its window.open/download/"+
			"fullscreen call is dropped while the click still reports success", params["userGesture"])
	}
}
