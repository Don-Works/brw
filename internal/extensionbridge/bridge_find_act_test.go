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
	"github.com/Don-Works/brw/internal/snapshot"
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
	mu sync.Mutex
	// elements is what the live in-page walk returns.
	elements []map[string]any
	// cached, when set, is what the extension's snapshot CACHE returns. The
	// cache-validity probe only sees DOM mutations, so it can legitimately hold
	// a page that a fill or a select has since changed.
	cached []map[string]any
	ran    []string
	// expressions records every evaluated script, so a test can assert which
	// options actually reached the page.
	expressions []string
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
		case "cached_snapshot":
			f.mu.Lock()
			cached := f.cached
			f.mu.Unlock()
			if cached == nil {
				break
			}
			f.note("cache")
			result = map[string]any{"cached": true, "snapshot": map[string]any{
				"url": "https://fixture.test/", "title": "Fixture", "elements": cached,
			}}
		case "cdp":
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			if method == "Runtime.evaluate" {
				expr, _ := params["expression"].(string)
				f.mu.Lock()
				f.expressions = append(f.expressions, expr)
				f.mu.Unlock()
				result = map[string]any{"result": map[string]any{"value": f.reply(expr)}}
			}
		}
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectFindActExtension(t *testing.T, b *Bridge, elements []map[string]any) (*findActExtension, func()) {
	t.Helper()
	return connectFindActExtensionWithCache(t, b, elements, nil)
}

func connectFindActExtensionWithCache(t *testing.T, b *Bridge, elements, cached []map[string]any) (*findActExtension, func()) {
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
	fe := &findActExtension{elements: elements, cached: cached}
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

// A locate-and-act must decide from the page as it is now. The extension's
// cache-validity probe only sees DOM mutations, and a fill, select or checkbox
// writes a DOM property that mutates no node, so an earlier step in the same
// batch can leave the cache holding the pre-action page. Resolving from it
// would let the exactly-one-match rule confirm a uniqueness the page no longer
// has, and then actuate on it.
func TestBridgeFindActResolvesFromTheLivePageNotTheCache(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectFindActExtensionWithCache(t,
		b,
		// Live: two rivals.
		[]map[string]any{
			fixtureElement("e1", "button", "Add to cart"),
			fixtureElement("e2", "button", "Add to wishlist"),
		},
		// Cache: the page before the second button appeared.
		[]map[string]any{fixtureElement("e1", "button", "Add to cart")},
	)
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
	if result.OK {
		t.Fatal("find_act resolved a unique match from the cached page and acted on it")
	}
	if !strings.Contains(result.Error, "refusing to guess") {
		t.Fatalf("batch error = %q, want the ambiguity the live page has", result.Error)
	}
	if fe.didRun("click") {
		t.Fatal("find_act actuated an element the live page says is ambiguous")
	}
	// An ordinary read may still be served from the cache; only the decision to
	// act is forced live.
	if _, err := b.Find(ctx, snapshot.FindOptions{Query: "Add"}); err != nil {
		t.Fatalf("read-only find: %v", err)
	}
	if !fe.didRun("cache") {
		t.Fatal("a read-only find stopped consulting the snapshot cache")
	}
}

// brw_find advertises text_content on every transport. Dropping it here made
// the option a no-op on the extension bridge: the tool said it would match
// visible prose and then matched only element metadata.
func TestBridgeFindPassesTextContentToThePage(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectFindActExtension(t, b, []map[string]any{fixtureElement("e1", "button", "Add to cart")})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := b.Find(ctx, snapshot.FindOptions{Query: "Add", TextContent: true}); err != nil {
		t.Fatalf("find: %v", err)
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()
	for _, expr := range fe.expressions {
		if strings.Contains(expr, snapshotScriptMarker) && strings.Contains(expr, `"text_content":true`) {
			return
		}
	}
	t.Fatalf("no page walk carried text_content; the option never left the daemon: %d expressions", len(fe.expressions))
}
