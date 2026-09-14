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

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// Markers that identify which in-page walker an evaluation is running, taken
// from strings unique to each script.
const (
	snapshotScriptMarker = "low_semantic_coverage"
	resolveBoxMarker     = "viewport_x"
	clickXYMarker        = "no element at coordinates"
)

// findActExtension is a fake extension that serves a fixed element list to the
// snapshot walker and records which walkers ran, so a batch's find_act step can
// be observed end to end over the real websocket path.
type findActExtension struct {
	mu       sync.Mutex
	elements []map[string]any
	ran      []string
}

func (f *findActExtension) note(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, kind)
}

func (f *findActExtension) didRun(kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, seen := range f.ran {
		if seen == kind {
			return true
		}
	}
	return false
}

func (f *findActExtension) reply(expr string) any {
	switch {
	case strings.Contains(expr, clickXYMarker):
		f.note("click")
		return map[string]any{"ok": true, "x": 10, "y": 20, "tag": "button"}
	case strings.Contains(expr, resolveBoxMarker):
		f.note("resolve")
		return map[string]any{
			"ok": true, "ref": "e1", "x": 10, "y": 20, "width": 80, "height": 24,
			"viewport_x": 50, "viewport_y": 32,
		}
	case strings.Contains(expr, snapshotScriptMarker):
		f.note("snapshot")
		f.mu.Lock()
		elements := f.elements
		f.mu.Unlock()
		return map[string]any{"url": "https://fixture.test/", "title": "Fixture", "elements": elements}
	default:
		return map[string]any{"ok": true}
	}
}

func (f *findActExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
				expr, _ := params["expression"].(string)
				result = map[string]any{"result": map[string]any{"value": f.reply(expr)}}
			}
		}
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectFindActExtension(t *testing.T, b *Bridge, elements []map[string]any) (*findActExtension, func()) {
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
	conn.SetReadLimit(8 << 20)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	fe := &findActExtension{elements: elements}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fe.serve(serveCtx, conn)
	return fe, func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

func fixtureElement(ref, role, name string) map[string]any {
	return map[string]any{"ref": ref, "role": role, "name": name, "tag": "button", "visible": true, "in_viewport": true}
}

// The extension transport must run a find_act batch step the same way the
// direct-CDP one does: resolve to exactly one element, then actuate it — and on
// several matches, actuate nothing.
func TestBridgeBatchFindActStep(t *testing.T) {
	tests := []struct {
		name      string
		elements  []map[string]any
		wantOK    bool
		wantClick bool
		wantErr   string
	}{
		{
			name:      "one match is clicked",
			elements:  []map[string]any{fixtureElement("e1", "button", "Add to cart")},
			wantOK:    true,
			wantClick: true,
		},
		{
			name: "two matches actuate nothing",
			elements: []map[string]any{
				fixtureElement("e1", "button", "Add to cart"),
				fixtureElement("e2", "button", "Add to wishlist"),
			},
			wantErr: "refusing to guess",
		},
		{
			name:     "no match actuates nothing",
			elements: []map[string]any{},
			wantErr:  "no element matches",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			fe, cleanup := connectFindActExtension(t, b, tt.elements)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			result, err := b.ExecuteBatch(ctx, []browser.BatchStep{{
				Action: "find_act",
				Find:   &browser.FindAct{Query: "Add", Role: "button", Action: "click"},
			}})
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			if tt.wantOK != result.OK {
				t.Fatalf("batch ok = %v, want %v (error %q)", result.OK, tt.wantOK, result.Error)
			}
			if tt.wantErr != "" && !strings.Contains(result.Error, tt.wantErr) {
				t.Fatalf("batch error = %q, want it to contain %q", result.Error, tt.wantErr)
			}
			if got := fe.didRun("click"); got != tt.wantClick {
				t.Fatalf("clicked = %v, want %v — the extension transport %s",
					got, tt.wantClick,
					map[bool]string{true: "actuated an element it should have refused", false: "refused an element it should have clicked"}[got])
			}
			if tt.wantOK && result.Steps[0].Ref != "e1" {
				t.Fatalf("step ref = %q, want e1", result.Steps[0].Ref)
			}
		})
	}
}
