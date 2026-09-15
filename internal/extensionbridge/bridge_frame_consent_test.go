package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

// frameCall is one read_cross_origin_frames message as it crossed the socket.
type frameCall struct {
	Origins    []string
	Expression string
}

// frameExtension is a fake service worker carrying two cross-origin iframes. It
// records every read_cross_origin_frames message and, like the real extension,
// evaluates only in the origins the message named.
type frameExtension struct {
	mu    sync.Mutex
	calls []frameCall
	cold  string
}

func (f *frameExtension) recorded() []frameCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]frameCall(nil), f.calls...)
}

var frameFixtureOrigins = []string{"https://payments.test", "https://widget.test"}

func (f *frameExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
			if expression != f.cold {
				reply["ok"] = false
				reply["error"] = "TypeError: window.__brw_snap is not a function"
				break
			}
			reply["result"] = map[string]any{"result": map[string]any{"value": map[string]any{
				"url": "https://fixture.test/", "title": "fixture",
				"elements": []map[string]any{{"ref": "e1", "role": "button", "name": "Buy", "tag": "button"}},
				"metadata": map[string]any{"version": 1, "cross_origin_frames": []map[string]any{
					{"x": 10, "y": 20, "width": 300, "height": 200, "origin": frameFixtureOrigins[0]},
					{"x": 10, "y": 300, "width": 300, "height": 200, "origin": frameFixtureOrigins[1]},
				}},
			}}}
		case "read_cross_origin_frames":
			call := frameCall{}
			if raw, ok := msg.Params["origins"].([]any); ok {
				call.Origins = []string{}
				for _, item := range raw {
					if text, isText := item.(string); isText {
						call.Origins = append(call.Origins, text)
					}
				}
			}
			call.Expression, _ = msg.Params["expression"].(string)
			f.mu.Lock()
			f.calls = append(f.calls, call)
			f.mu.Unlock()
			allowed := map[string]bool{}
			for _, origin := range call.Origins {
				allowed[origin] = true
			}
			frames := make([]map[string]any, 0, len(frameFixtureOrigins))
			for _, origin := range frameFixtureOrigins {
				frame := map[string]any{"url": origin + "/embed", "origin": origin}
				// The real extension evaluates only where it was told to.
				if allowed[origin] && call.Expression != "" {
					frame["snapshot"] = map[string]any{
						"url": origin + "/embed",
						"elements": []map[string]any{
							{"ref": "e1", "role": "button", "name": "Pay", "tag": "button", "x": 5, "y": 5, "w": 60, "h": 20},
						},
					}
				}
				frames = append(frames, frame)
			}
			reply["result"] = map[string]any{"frames": frames}
		}
		out, _ := json.Marshal(reply)
		_ = conn.Write(ctx, websocket.MessageText, out)
	}
}

func connectFrameExtension(t *testing.T, b *Bridge, f *frameExtension) func() {
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
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go f.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// TestIncludeFramesReadsOnlyTheOriginsConsentAllows is the consent half of
// include_frames on the bridge.
//
// brw_snapshot is granted against the origin the TAB is showing. include_frames
// then runs brw's walker inside every cross-origin iframe of that page — the
// payment form, the embedded editor — which is a read of a THIRD PARTY's
// document that the embedder's grant never covered and the call's arguments
// never named. So the extension is asked WHICH origins are embedded before any
// expression is sent, each is put to the gate on its own, and the expression goes
// out naming only the ones that passed.
func TestIncludeFramesReadsOnlyTheOriginsConsentAllows(t *testing.T) {
	frameOpts := snapshot.SnapshotOptions{Mode: "all"}
	frameOpts.IncludeBoxes = true
	_, frameCold := snapshot.SnapshotCallExpressions(frameOpts)

	cases := []struct {
		name        string
		allow       browser.FrameReadCheck
		wantOrigins []string
		wantRead    []string
	}{
		{
			name:        "no gate installed reads every embedded origin",
			wantOrigins: frameFixtureOrigins,
			wantRead:    frameFixtureOrigins,
		},
		{
			name: "a refused origin is never named to the extension",
			allow: func(origin string) error {
				if origin == "https://payments.test" {
					return errors.New("no site permission grant for https://payments.test (scope read)")
				}
				return nil
			},
			wantOrigins: []string{"https://widget.test"},
			wantRead:    []string{"https://widget.test"},
		},
		{
			name:        "every origin refused sends no expression at all",
			allow:       func(string) error { return errors.New("refused") },
			wantOrigins: nil,
			wantRead:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true}
			_, cold := snapshot.SnapshotCallExpressions(opts)

			b := New("", 5*time.Second, "")
			ext := &frameExtension{cold: cold}
			cleanup := connectFrameExtension(t, b, ext)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ctx = browser.WithFrameReadCheck(ctx, tc.allow)

			snap, err := b.Snapshot(ctx, opts)
			if err != nil {
				t.Fatalf("snapshot with include_frames: %v", err)
			}

			calls := ext.recorded()
			if len(calls) == 0 {
				t.Fatal("include_frames sent no read_cross_origin_frames message")
			}
			// The first message must carry no expression: nothing runs in a third
			// party's document before the daemon has decided about its origin.
			if calls[0].Origins != nil || calls[0].Expression != "" {
				t.Fatalf("the first frame message already carried an expression for %v; the origins were not decided first", calls[0].Origins)
			}
			var named []string
			for _, call := range calls[1:] {
				if call.Expression == "" {
					t.Fatalf("a second-phase frame message carried no expression: %+v", call)
				}
				if call.Expression != frameCold {
					t.Fatal("the frame message did not carry the shared walker; a second extractor would mint refs nothing guards")
				}
				named = append(named, call.Origins...)
			}
			if !sameOrigins(named, tc.wantOrigins) {
				t.Fatalf("the expression was sent for %v, want %v", named, tc.wantOrigins)
			}

			// And what came back matches: a refused frame is still REPORTED, as the
			// clickable box it was before include_frames could read it at all.
			read := readFrameOrigins(snap.Elements)
			if !sameOrigins(read, tc.wantRead) {
				t.Fatalf("controls were merged from %v, want %v", read, tc.wantRead)
			}
			for _, origin := range frameFixtureOrigins {
				if contains(read, origin) || mentionsOrigin(snap.Elements, origin) {
					continue
				}
				t.Fatalf("frame %s is neither read nor surfaced as a clickable box; a frame brw will not read must still be visible to the agent", origin)
			}
		})
	}
}

// readFrameOrigins lists the origins whose CONTROLS were merged, by the frame
// index their refs carry.
func readFrameOrigins(elements []snapshot.Element) []string {
	var out []string
	for _, el := range elements {
		if !snapshot.IsCrossOriginElementRef(el.Ref) {
			continue
		}
		index, _, _ := snapshot.ParseFrameRef(el.Ref)
		if index >= 0 && index < len(frameFixtureOrigins) {
			out = append(out, frameFixtureOrigins[index])
		}
	}
	return out
}

func mentionsOrigin(elements []snapshot.Element, origin string) bool {
	for _, el := range elements {
		if strings.Contains(el.Name, origin) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func sameOrigins(got, want []string) bool {
	seen := map[string]bool{}
	for _, origin := range got {
		seen[origin] = true
	}
	if len(seen) != len(want) {
		return false
	}
	for _, origin := range want {
		if !seen[origin] {
			return false
		}
	}
	return true
}
