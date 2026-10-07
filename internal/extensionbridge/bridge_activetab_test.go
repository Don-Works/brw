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

type fakeExtension struct {
	mu sync.Mutex

	tabs []fakeTab

	focusedWindow int

	agentTabId int

	noDrivableReason string
}

type fakeTab struct {
	id       int
	windowID int
	active   bool
	url      string
	title    string
}

func (f *fakeExtension) foregroundID() int {
	if f.agentTabId != 0 {
		for _, t := range f.tabs {
			if t.id == f.agentTabId {
				return f.agentTabId
			}
		}
	}
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
			"windowFocused": isFg,
			"windowType":    "normal",
		})
	}
	return out
}

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

	f.agentTabId = tabID
	return true
}

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
			if f.noDrivableReason != "" {
				result = map[string]any{"tabId": 0, "error": f.noDrivableReason}
			} else {
				result = map[string]any{"tabId": f.foregroundID()}
			}
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

func TestNoDrivableTabDoesNotFallBackToTheCachedTab(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectFakeExtension(t, b)
	defer cleanup()
	ctx := context.Background()

	if got := b.contextTabID(ctx); got != "12" {
		t.Fatalf("contextTabID = %q, want 12", got)
	}
	if cached := b.activeTabID(); cached != "12" {
		t.Fatalf("cached active tab = %q, want 12", cached)
	}

	fe.mu.Lock()
	fe.noDrivableReason = "no drivable tab: the active tab is " +
		"chrome-extension://nngceckbapebfimnlniiiahkandclblb/popup/index.html, " +
		"which Chrome does not allow brw to control. Switch to a normal page tab, or pass an explicit tab_id."
	fe.mu.Unlock()

	if got := b.contextTabID(ctx); got != "" {
		t.Fatalf("contextTabID = %q after a definitive no-drivable-tab answer, want \"\" so the call surfaces the real reason instead of retargeting the stale cached tab", got)
	}

	if cached := b.activeTabID(); cached != "12" {
		t.Fatalf("cached active tab = %q, want it preserved as 12", cached)
	}

	fe.mu.Lock()
	fe.noDrivableReason = ""
	fe.focusedWindow = 999
	fe.agentTabId = 0
	fe.mu.Unlock()

	if got := b.contextTabID(ctx); got != "12" {
		t.Fatalf("contextTabID = %q on a transient resolution failure, want the cached 12", got)
	}

	fe.mu.Lock()
	fe.focusedWindow = 2
	fe.mu.Unlock()
	if got := b.contextTabID(ctx); got != "12" {
		t.Fatalf("contextTabID = %q after recovery, want 12", got)
	}
}

func TestActiveTabResolutionIsConsistentAcrossPageTools(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := connectFakeExtension(t, b)
	defer cleanup()

	ctx := context.Background()

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

	for i, tool := range []string{"read", "observe", "snapshot", "find", "click"} {
		got := b.contextTabID(ctx)
		if got != listActive {
			t.Fatalf("%s (call %d) resolved tab %q, want %q (must match list_tabs active)", tool, i, got, listActive)
		}
	}

	if got := b.resolveActiveTabID(ctx); got != listActive {
		t.Fatalf("resolveActiveTabID = %q, want %q (must match list_tabs active)", got, listActive)
	}
}

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

func TestServiceWorkerActiveTabResolverIsAuthoritative(t *testing.T) {
	src := readServiceWorker(t)
	for _, want := range []string{
		"function resolveForegroundTabId(",
		"const foregroundId = await resolveForegroundTabId()",
		"summary.active = isForeground",
		"const id = await resolveForegroundTabId();",
		"const makeActive = message.params?.active !== false;",
		"const normalWindowId = await preferredNormalWindowId();",
		"tab = await chrome.tabs.create(createParams);",
		"state.agentTabId = tab.id || null;",

		"if (!/no current window/i.test(String(err?.message || err))) throw err;",
		"const win = await chrome.windows.create({",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker authoritative active-tab resolver missing %q", want)
		}
	}

	resolver := sliceBetween(src, "async function resolveForegroundTabId()", "async function activeTabId()")
	focusIdx := strings.Index(resolver, "win.focused")
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
