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

// groupAwareExtension is a richer in-memory stand-in for the Chrome extension
// service worker than fakeExtension: it additionally models tab GROUPS and the
// real-Chrome invariant that a COLLAPSED group cannot hold the active tab. It
// answers exactly the RPCs the brw_open path exercises end-to-end: open_tab, the
// readiness-wait cdp Runtime.evaluate, get_active_tab_id, and focus_tab. It is
// the harness for the "brw_open then no-tab_id tool hit an unrelated tab"
// regression.
type groupAwareExtension struct {
	mu            sync.Mutex
	tabs          []*gaTab
	groups        map[int]*gaGroup
	focusedWindow int
	nextTabID     int
}

type gaTab struct {
	id       int
	windowID int
	groupID  int // -1 = ungrouped
	active   bool
	url      string
	title    string
}

type gaGroup struct {
	id        int
	windowID  int
	title     string
	collapsed bool
}

// foregroundID mirrors service_worker resolveForegroundTabId(): the active tab
// of the focused window — the single source of truth get_active_tab_id returns.
func (f *groupAwareExtension) foregroundID() int {
	for _, t := range f.tabs {
		if t.windowID == f.focusedWindow && t.active {
			return t.id
		}
	}
	return 0
}

func (f *groupAwareExtension) tabByID(id int) *gaTab {
	for _, t := range f.tabs {
		if t.id == id {
			return t
		}
	}
	return nil
}

// activateExclusive makes id the sole active tab of its window.
func (f *groupAwareExtension) activateExclusive(windowID, id int) {
	for _, t := range f.tabs {
		if t.windowID == windowID {
			t.active = t.id == id
		}
	}
}

// firstVisibleOther returns a tab in windowID, other than exclude, that is NOT
// hidden inside a collapsed group — the candidate Chrome activates when the
// active tab is forced into a collapsed group.
func (f *groupAwareExtension) firstVisibleOther(windowID, exclude int) *gaTab {
	for _, t := range f.tabs {
		if t.windowID != windowID || t.id == exclude {
			continue
		}
		if t.groupID >= 0 {
			if g := f.groups[t.groupID]; g != nil && g.collapsed {
				continue
			}
		}
		return t
	}
	return nil
}

func (f *groupAwareExtension) groupByTitle(windowID int, title string) *gaGroup {
	for _, g := range f.groups {
		if g.windowID == windowID && g.title == title {
			return g
		}
	}
	return nil
}

func (f *groupAwareExtension) summary(t *gaTab) map[string]any {
	return map[string]any{
		"id":            t.id,
		"url":           t.url,
		"title":         t.title,
		"active":        t.active,
		"windowId":      t.windowID,
		"windowFocused": t.windowID == f.focusedWindow && t.active,
		"windowType":    "normal",
		"groupId":       t.groupID,
	}
}

func (f *groupAwareExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
		case "get_active_tab_id":
			result = map[string]any{"tabId": f.foregroundID()}
		case "cdp":
			// The only cdp call on the open path is the readiness-wait
			// Runtime.evaluate (condition "committed"); report it satisfied.
			result = map[string]any{"result": map[string]any{"value": true}}
		case "open_tab":
			result = f.handleOpen(msg.Params)
		case "focus_tab":
			id := paramInt(msg.Params, "tabId")
			t := f.tabByID(id)
			if t == nil {
				ok = false
				result = map[string]any{}
				break
			}
			// focus_tab focuses the window, EXPANDS the target's group (a real
			// service worker must, else the activate cannot stick), and activates
			// the tab — exactly the heal path ensureForegroundTab relies on.
			f.focusedWindow = t.windowID
			if t.groupID >= 0 {
				if g := f.groups[t.groupID]; g != nil {
					g.collapsed = false
				}
			}
			f.activateExclusive(t.windowID, t.id)
			result = f.summary(t)
		default:
			result = map[string]any{}
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": ok, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

// handleOpen models chrome.tabs.create({active:true}) followed by grouping,
// including the collapsed-group deactivation that demotes the freshly-opened
// active tab — the concrete mechanism behind the reported bug.
func (f *groupAwareExtension) handleOpen(params map[string]any) map[string]any {
	id := f.nextTabID
	f.nextTabID++
	url, _ := params["url"].(string)
	t := &gaTab{id: id, windowID: f.focusedWindow, groupID: -1, active: true, url: url, title: url}
	f.tabs = append(f.tabs, t)
	// active:true within its window.
	f.activateExclusive(t.windowID, t.id)

	groupName, _ := params["groupName"].(string)
	if strings.TrimSpace(groupName) != "" {
		g := f.groupByTitle(t.windowID, groupName)
		if g == nil {
			g = &gaGroup{id: 9000 + len(f.groups), windowID: t.windowID, title: groupName, collapsed: false}
			f.groups[g.id] = g
		}
		t.groupID = g.id
		if g.collapsed {
			// A collapsed group cannot show the active tab: Chrome deactivates the
			// newcomer and activates an adjacent visible tab instead.
			t.active = false
			if other := f.firstVisibleOther(t.windowID, t.id); other != nil {
				other.active = true
			}
		}
	}
	return f.summary(t)
}

func paramInt(params map[string]any, key string) int {
	switch n := params[key].(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	}
	return 0
}

func connectGroupAwareExtension(t *testing.T, b *Bridge, fe *groupAwareExtension) func() {
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
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fe.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// TestOpenInGroupMakesOpenedTabForeground is the regression for the reported
// bug: brw_open opened a localhost tab into a (collapsed) group, but the
// subsequent no-tab_id tool resolved an UNRELATED existing tab (Google Chat)
// because grouping into a collapsed group demoted the freshly-opened active tab.
// After the fix, OpenInGroup must make the opened tab the genuine foreground tab
// so every subsequent no-tab_id tool (modelled by contextTabID) targets it.
func TestOpenInGroupMakesOpenedTabForeground(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     200,
		groups: map[int]*gaGroup{
			9: {id: 9, windowID: 1, title: "mcplexer-ui-ux-pass", collapsed: true},
		},
		tabs: []*gaTab{
			{id: 100, windowID: 1, groupID: -1, active: true, url: "https://chat.google.com/", title: "Google Chat"},
			{id: 102, windowID: 1, groupID: 9, active: false, url: "http://127.0.0.1:13333/old", title: "prior localhost"},
		},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	const target = "http://127.0.0.1:13333/workspaces/routes?ux_check=1"
	res, err := b.OpenInGroup(ctx, target, browser.TabGroupOptions{Name: "mcplexer-ui-ux-pass"})
	if err != nil {
		t.Fatalf("OpenInGroup: %v", err)
	}
	if res.Tab.ID != "200" {
		t.Fatalf("OpenInGroup returned tab %q, want 200 (the freshly opened tab)", res.Tab.ID)
	}

	// THE BUG: without making the opened tab foreground, get_active_tab_id resolves
	// the Google Chat tab (100) that Chrome re-activated when the new tab was
	// demoted into the collapsed group — so brw_find/read/click would all hit it.
	if got := b.contextTabID(ctx); got != "200" {
		t.Fatalf("after brw_open, contextTabID resolved %q, want 200 — a subsequent no-tab_id tool would act on the wrong tab", got)
	}
	if got := b.resolveActiveTabID(ctx); got != "200" {
		t.Fatalf("after brw_open, resolveActiveTabID = %q, want 200", got)
	}
}

// TestEnsureForegroundTabErrorsWithoutTabID proves brw_open never silently falls
// back to a stale active tab: an empty opened-tab id is an explicit error.
func TestEnsureForegroundTabErrorsWithoutTabID(t *testing.T) {
	b := New("", time.Second, "")
	if err := b.ensureForegroundTab(context.Background(), ""); err == nil {
		t.Fatal("ensureForegroundTab(\"\") = nil, want explicit error rather than a stale-tab fallback")
	}
}

// TestServiceWorkerOpenRehydratesForeground locks in the extension-side
// hardening: open_tab re-expands a collapsed group and re-activates the opened
// tab after grouping so the opened tab stays the foreground tab at the source.
func TestServiceWorkerOpenRehydratesForeground(t *testing.T) {
	src := readServiceWorker(t)
	for _, want := range []string{
		"chrome.tabGroups.update(groupId, { collapsed: false })", // open re-expands a collapsed group
		"chrome.tabs.update(tab.id, { active: true })",           // open re-activates after grouping
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker open_tab must re-assert the opened tab as foreground; missing %q", want)
		}
	}
}
