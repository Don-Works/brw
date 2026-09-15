package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

// propertyWriteFake models the extension whose snapshot cache never goes stale
// on a form-control write. value/selected/checked live in DOM PROPERTIES, so a
// fill or a select mutates no node, the in-page MutationObserver never fires,
// and cached_snapshot keeps answering with whatever was last stored. Anything
// that reads the page through that cache after such an action sees the
// pre-action page.
type propertyWriteFake struct {
	mu sync.Mutex
	// live is the page's real field value; cached is the value frozen in the
	// extension's per-tab snapshot cache.
	live     string
	cached   string
	hasCache bool
	// walks counts in-page snapshot walks, so a test can prove the observation
	// re-read the page rather than being answered from the cache.
	walks int
}

func (f *propertyWriteFake) snapshot(value string) map[string]any {
	return map[string]any{
		"url":   "https://fixture.test/form",
		"title": "form",
		"elements": []any{map[string]any{
			"ref": "e1", "role": "textbox", "name": "Email", "tag": "input",
			"type": "text", "value": value, "visible": true,
		}},
		"metadata": map[string]any{"version": 1, "focused_ref": "e1"},
	}
}

func (f *propertyWriteFake) serve(ctx context.Context, conn *websocket.Conn) {
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
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		var result any = map[string]any{}
		f.mu.Lock()
		switch msg.Type {
		case "get_active_tab_id":
			result = map[string]any{"tabId": 7}
		case "list_tabs":
			result = []any{map[string]any{
				"id": 7, "url": "https://fixture.test/form", "title": "form",
				"active": true, "windowId": 1,
			}}
		case "cached_snapshot":
			if f.hasCache {
				result = map[string]any{"cached": true, "snapshot": f.snapshot(f.cached)}
			} else {
				result = map[string]any{"cached": false}
			}
		case "snapshot_result":
			f.cached = f.live
			f.hasCache = true
			result = map[string]any{"stored": true}
		case "cdp":
			expression, _ := msg.Params["params"].(map[string]any)["expression"].(string)
			switch {
			case isSnapshotWalkExpression(expression):
				// the in-page snapshot walker
				f.walks++
				result = map[string]any{"result": map[string]any{"value": f.snapshot(f.live)}}
			case strings.HasPrefix(expression, "(function(ref"):
				// fill / select: writes the property, mutates no node
				f.live = "beta@example.test"
				result = map[string]any{"result": map[string]any{"value": map[string]any{
					"ok": true, "ref": "e1", "value": f.live,
				}}}
			default:
				// settle fingerprint: a page that is already quiescent
				result = map[string]any{"result": map[string]any{"value": "complete|1|1|INPUT#email|https://fixture.test/form"}}
			}
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectPropertyWriteFake(t *testing.T) (*Bridge, *propertyWriteFake, func()) {
	t.Helper()
	b := New("", 4*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		srv.Close()
		t.Fatalf("dial property-write fake: %v", err)
	}
	// The snapshot walker arrives as one large expression.
	conn.SetReadLimit(4 << 20)
	waitUntil(t, b.liveConn)
	fake := &propertyWriteFake{live: "alpha@example.test"}
	serveCtx, cancel := context.WithCancel(context.Background())
	go fake.serve(serveCtx, conn)
	return b, fake, func() {
		cancel()
		_ = conn.CloseNow()
		srv.Close()
	}
}

// An action that writes only a DOM property must still be observed against the
// live page. Answering the post-action observation from the extension's tab
// cache reported changed_state:false with the pre-action value, and an agent
// that believes a fill failed retries it — writing the value twice on a real
// form.
func TestObservedActionReadsPastTheTabSnapshotCache(t *testing.T) {
	tests := []struct {
		name string
		act  func(context.Context, *Bridge) (browser.ActionResult, error)
	}{
		{
			name: "fill",
			act: func(ctx context.Context, b *Bridge) (browser.ActionResult, error) {
				return b.Fill(ctx, snapshot.FillOptions{Ref: "e1", Text: "beta@example.test", Replace: true})
			},
		},
		{
			name: "select",
			act: func(ctx context.Context, b *Bridge) (browser.ActionResult, error) {
				return b.Select(ctx, "e1", "beta@example.test")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, fake, cleanup := connectPropertyWriteFake(t)
			defer cleanup()
			ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "7"), 5*time.Second)
			defer cancel()

			// Prime the cache the way any earlier read would, so the observation
			// below has a cache entry to be wrongly served from.
			if _, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true}); err != nil {
				t.Fatalf("prime snapshot: %v", err)
			}
			fake.mu.Lock()
			walksBefore := fake.walks
			fake.mu.Unlock()

			result, err := tc.act(ctx, b)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			fake.mu.Lock()
			walksAfter, live := fake.walks, fake.live
			fake.mu.Unlock()

			if walksAfter == walksBefore {
				t.Fatal("post-action observation never re-walked the page; it was served from the tab snapshot cache")
			}
			if result.ChangedState == nil || !*result.ChangedState {
				t.Fatalf("changed_state = %v, want true (the field now holds %q)", result.ChangedState, live)
			}
			if strings.Contains(result.Warning, "no observable semantic state change") {
				t.Fatalf("warning = %q, want no stale-state warning", result.Warning)
			}
			if len(result.Elements) != 1 || result.Elements[0].Value != live {
				t.Fatalf("observed elements = %+v, want the post-action value %q", result.Elements, live)
			}
		})
	}
}

// The in-page dirty flag backing cached_snapshot decides whether a later read
// re-walks the page. A MutationObserver cannot see a value, checked or
// selectedIndex write, so the observer must also listen for input/change.
// extension/tab_resolution_test.mjs executes the injected script; this is the
// backstop for an environment without node.
func TestServiceWorkerObserverMarksFormControlWritesDirty(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	worker := string(data)
	for _, want := range []string{"new MutationObserver", "'input'", "'change'", "document.addEventListener"} {
		if !strings.Contains(worker, want) {
			t.Fatalf("service worker observer is missing %q; a form-control write would leave the cached snapshot marked clean", want)
		}
	}
}
