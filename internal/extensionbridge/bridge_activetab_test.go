package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeExtension is an in-memory stand-in for the Chrome extension service
// worker. It models a multi-window/multi-tab browser and answers the bridge's
// list_tabs / get_active_tab_id / focus_tab RPCs from ONE shared tab model the
// way the real service_worker now does: a single authoritative foreground tab
// (the active tab of the focused window) backs BOTH the list_tabs active flag
// AND get_active_tab_id, so the two can never disagree.
type fakeExtension struct {
	mu sync.Mutex
	// tabs is ordered; each has an id, windowId, and per-window active flag.
	tabs []fakeTab
	// focusedWindow is the id of the OS-focused window.
	focusedWindow int
}

type fakeTab struct {
	id       int
	windowID int
	active   bool // active within its window
	url      string
	title    string
}

// foregroundID mirrors service_worker resolveForegroundTabId(): the active tab
// of the focused window. This is the single source of truth.
func (f *fakeExtension) foregroundID() int {
	for _, t := range f.tabs {
		if t.windowID == f.focusedWindow && t.active {
			return t.id
		}
	}
	return 0
}

func (f *fakeExtension) listTabs() []map[string]any {
	fg := f.foregroundID()
	out := make([]map[string]any, 0, len(f.tabs))
	for _, t := range f.tabs {
		isFg := t.id == fg
		out = append(out, map[string]any{
			"id":            t.id,
			"url":           t.url,
			"title":         t.title,
			"active":        isFg,
			"windowId":      t.windowID,
			"windowFocused": isFg, // forced true for the foreground tab, like the SW
			"windowType":    "normal",
		})
	}
	return out
}

// focus makes tabID the active tab of its window and focuses that window —
// matching the service_worker focus_tab handler.
func (f *fakeExtension) focus(tabID int) bool {
	var win int
	found := false
	for _, t := range f.tabs {
		if t.id == tabID {
			win = t.windowID
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for i := range f.tabs {
		if f.tabs[i].windowID == win {
			f.tabs[i].active = f.tabs[i].id == tabID
		}
	}
	f.focusedWindow = win
	return true
}

// serve runs the fake extension against the bridge's websocket until ctx is
// done. It answers exactly the RPCs the active-tab resolution paths use.
func (f *fakeExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
		f.mu.Lock()
		var result any
		ok := true
		switch msg.Type {
		case "list_tabs":
			result = f.listTabs()
		case "list_tab_groups":
			result = []map[string]any{{
				"id":        9,
				"title":     "workspace-2",
				"color":     "cyan",
				"collapsed": false,
				"windowId":  2,
				"tabIds":    []int{12, 13},
				"tabCount":  2,
			}}
		case "get_active_tab_id":
			result = map[string]any{"tabId": f.foregroundID()}
		case "focus_tab":
			id := 0
			if v, found := msg.Params["tabId"]; found {
				switch n := v.(type) {
				case float64:
					id = int(n)
				case json.Number:
					i, _ := n.Int64()
					id = int(i)
				}
			}
			if f.focus(id) {
				result = map[string]any{"id": id, "active": true, "windowId": f.focusedWindow, "windowType": "normal"}
			} else {
				ok = false
				result = map[string]any{}
			}
		default:
			result = map[string]any{}
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": ok, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectFakeExtension(t *testing.T, b *Bridge) (*fakeExtension, func()) {
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

	fe := &fakeExtension{
		// A 20-tab, two-window model mirroring the live-proof session. Window 2 is
		// focused; its active tab (id 12, "Men's Carbon running shoes") is the one
		// authoritative foreground tab.
		focusedWindow: 2,
		tabs: []fakeTab{
			{id: 1, windowID: 1, active: false, url: "https://a.test/1", title: "Kids' Running Shoes"},
			{id: 2, windowID: 1, active: true, url: "https://a.test/2", title: "Intervals Pro"},
			{id: 11, windowID: 2, active: false, url: "https://shop.test/11", title: "Trail Shoes"},
			{id: 12, windowID: 2, active: true, url: "https://shop.test/12", title: "Men's Carbon running shoes"},
			{id: 13, windowID: 2, active: false, url: "https://shop.test/13", title: "Socks"},
		},
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fe.serve(serveCtx, conn)

	cleanup := func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
	return fe, cleanup
}

// TestActiveTabResolutionIsConsistentAcrossPageTools is the regression test for
// the live-proof bug: three back-to-back no-tab_id calls each resolved to a
// DIFFERENT tab. It proves the daemon's single authoritative resolver
// (contextTabID — the one every page tool funnels through for the no-tab_id
// case) agrees with what ListTabs marks active, and is stable across repeated
// calls and across multiple distinct resolutions (read vs observe vs snapshot
// all call contextTabID, so resolving it repeatedly models them).
func TestActiveTabResolutionIsConsistentAcrossPageTools(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := connectFakeExtension(t, b)
	defer cleanup()

	ctx := context.Background()

	// What list_tabs reports as the active tab.
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	listActive := ""
	activeCount := 0
	for _, tab := range tabs {
		if tab.Active && tab.WindowFocused {
			listActive = tab.ID
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("list_tabs must mark exactly one focused-window active tab, got %d", activeCount)
	}
	if listActive != "12" {
		t.Fatalf("list_tabs active = %q, want 12 (focused window's active tab)", listActive)
	}

	// Every page tool resolves the no-tab_id target via contextTabID. Resolve it
	// repeatedly (modelling read, then observe, then snapshot, then find) and
	// require every resolution to equal the SAME tab list_tabs marks active.
	for i, tool := range []string{"read", "observe", "snapshot", "find", "click"} {
		got := b.contextTabID(ctx)
		if got != listActive {
			t.Fatalf("%s (call %d) resolved tab %q, want %q (must match list_tabs active)", tool, i, got, listActive)
		}
	}

	// And get_active_tab_id (the bridge's resolveActiveTabID) must agree too.
	if got := b.resolveActiveTabID(ctx); got != listActive {
		t.Fatalf("resolveActiveTabID = %q, want %q (must match list_tabs active)", got, listActive)
	}
}

// TestFocusTabAcceptsListTabsID proves the exact id list_tabs returns is
// accepted by focus_tab (the user reported these ids "don't exist"), and that
// after focusing, every no-tab_id page tool follows to the focused tab — still
// agreeing with list_tabs.
func TestFocusTabAcceptsListTabsID(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectFakeExtension(t, b)
	defer cleanup()
	_ = fe

	ctx := context.Background()
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}

	// Pick a non-active tab id straight from the list_tabs output and focus it.
	target := ""
	for _, tab := range tabs {
		if !(tab.Active && tab.WindowFocused) {
			target = tab.ID
			break
		}
	}
	if target == "" {
		t.Fatal("expected a non-active tab to focus")
	}
	if _, err := strconv.Atoi(target); err != nil {
		t.Fatalf("list_tabs returned a non-numeric id %q the extension cannot focus", target)
	}

	if err := b.FocusTab(ctx, target); err != nil {
		t.Fatalf("FocusTab(%q) from a list_tabs id failed: %v", target, err)
	}

	// After focus, list_tabs and every no-tab_id page tool must follow to it.
	tabs, err = b.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs after focus: %v", err)
	}
	listActive := ""
	for _, tab := range tabs {
		if tab.Active && tab.WindowFocused {
			listActive = tab.ID
		}
	}
	if listActive != target {
		t.Fatalf("after FocusTab(%q), list_tabs active = %q, want %q", target, listActive, target)
	}
	if got := b.contextTabID(ctx); got != target {
		t.Fatalf("after FocusTab(%q), contextTabID = %q, want %q (page tools must follow focus)", target, got, target)
	}
}

func TestListTabGroupsUsesExtensionPayload(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := connectFakeExtension(t, b)
	defer cleanup()

	groups, err := b.ListTabGroups(context.Background())
	if err != nil {
		t.Fatalf("ListTabGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	group := groups[0]
	if group.ID != "9" || group.Title != "workspace-2" || group.Color != "cyan" || group.WindowID != 2 || group.TabCount != 2 {
		t.Fatalf("unexpected group: %+v", group)
	}
	if len(group.TabIDs) != 2 || group.TabIDs[0] != "12" || group.TabIDs[1] != "13" {
		t.Fatalf("unexpected group tabs: %+v", group.TabIDs)
	}
}

// TestServiceWorkerActiveTabResolverIsAuthoritative guards the service_worker
// contract that backs the fix: get_active_tab_id and list_tabs derive their
// active tab from the SAME resolveForegroundTabId(), the cache is not trusted
// ahead of the focused-window scan, and open_tab creates the tab active so the
// agent follows it.
func TestServiceWorkerActiveTabResolverIsAuthoritative(t *testing.T) {
	src := readServiceWorker(t)
	for _, want := range []string{
		"function resolveForegroundTabId(",
		"const foregroundId = await resolveForegroundTabId()",                             // list_tabs uses the shared resolver
		"summary.active = isForeground",                                                   // list_tabs marks exactly the foreground tab
		"const id = await resolveForegroundTabId();",                                      // activeTabId() / get_active_tab_id uses it
		`chrome.tabs.create({ url: message.params?.url || "about:blank", active: true })`, // open follows
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker authoritative active-tab resolver missing %q", want)
		}
	}
	// The cache must NOT be returned ahead of the focused-window scan inside
	// resolveForegroundTabId — that ordering was the divergence root cause. The
	// focused-window loop must appear before the state.activeTabId fallback.
	resolver := sliceBetween(src, "async function resolveForegroundTabId()", "async function activeTabId()")
	focusIdx := strings.Index(resolver, "if (!win.focused) continue;")
	cacheIdx := strings.Index(resolver, "state.activeTabId")
	if focusIdx < 0 || cacheIdx < 0 || focusIdx > cacheIdx {
		t.Fatal("resolveForegroundTabId must scan the focused window BEFORE falling back to the cached tab id")
	}
}

func readServiceWorker(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sliceBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}
