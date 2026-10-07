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

type groupAwareExtension struct {
	mu            sync.Mutex
	tabs          []*gaTab
	groups        map[int]*gaGroup
	focusedWindow int
	nextTabID     int

	lastOpenGroupName    string
	lastOpenBackground   bool
	lastFocusRaiseWindow bool

	failOpen  bool
	openCalls int

	frameURL string

	navOutcome map[string]any

	frameTreeHangs bool
}

type gaTab struct {
	id       int
	windowID int
	groupID  int
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

func (f *groupAwareExtension) activateExclusive(windowID, id int) {
	for _, t := range f.tabs {
		if t.windowID == windowID {
			t.active = t.id == id
		}
	}
}

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
			method, _ := msg.Params["method"].(string)
			if method == "Page.getFrameTree" && f.frameTreeHangs {
				f.mu.Unlock()
				continue
			}
			if method == "Page.getFrameTree" && f.frameURL != "" {
				result = map[string]any{"frameTree": map[string]any{"frame": map[string]any{
					"id": "frame-main", "loaderId": "loader-main", "url": f.frameURL,
				}}}
				break
			}

			result = map[string]any{"result": map[string]any{"value": true}}
		case "navigation_outcome":
			if f.navOutcome == nil {
				ok = false
				result = map[string]any{}
				break
			}
			result = f.navOutcome
		case "open_tab":
			f.openCalls++
			if f.failOpen {
				ok = false
				result = map[string]any{}
				break
			}
			result = f.handleOpen(msg.Params)
		case "focus_tab":
			id := paramInt(msg.Params, "tabId")
			raise, _ := msg.Params["raiseWindow"].(bool)
			f.lastFocusRaiseWindow = raise
			t := f.tabByID(id)
			if t == nil {
				ok = false
				result = map[string]any{}
				break
			}

			if raise {
				f.focusedWindow = t.windowID
			}
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

func (f *groupAwareExtension) handleOpen(params map[string]any) map[string]any {
	id := f.nextTabID
	f.nextTabID++
	url, _ := params["url"].(string)

	active := true
	if v, ok := params["active"].(bool); ok {
		active = v
	}
	f.lastOpenBackground = !active
	t := &gaTab{id: id, windowID: f.focusedWindow, groupID: -1, active: active, url: url, title: url}
	f.tabs = append(f.tabs, t)
	if active {

		f.activateExclusive(t.windowID, t.id)
	}

	groupName, _ := params["groupName"].(string)
	f.lastOpenGroupName = groupName
	if strings.TrimSpace(groupName) != "" {
		g := f.groupByTitle(t.windowID, groupName)
		if g == nil {
			g = &gaGroup{id: 9000 + len(f.groups), windowID: t.windowID, title: groupName, collapsed: false}
			f.groups[g.id] = g
		}
		t.groupID = g.id
		if active && g.collapsed {

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

func TestOpenInGroupMakesOpenedTabForeground(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     200,
		groups: map[int]*gaGroup{
			9: {id: 9, windowID: 1, title: "brw-ui-ux-pass", collapsed: true},
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
	res, err := b.OpenInGroup(ctx, target, browser.TabGroupOptions{Name: "brw-ui-ux-pass"})
	if err != nil {
		t.Fatalf("OpenInGroup: %v", err)
	}
	if res.Tab.ID != "200" {
		t.Fatalf("OpenInGroup returned tab %q, want 200 (the freshly opened tab)", res.Tab.ID)
	}

	if got := b.contextTabID(ctx); got != "200" {
		t.Fatalf("after brw_open, contextTabID resolved %q, want 200 — a subsequent no-tab_id tool would act on the wrong tab", got)
	}
	if got := b.resolveActiveTabID(ctx); got != "200" {
		t.Fatalf("after brw_open, resolveActiveTabID = %q, want 200", got)
	}
}

func TestEnsureForegroundTabErrorsWithoutTabID(t *testing.T) {
	b := New("", time.Second, "")
	if err := b.ensureForegroundTab(context.Background(), ""); err == nil {
		t.Fatal("ensureForegroundTab(\"\") = nil, want explicit error rather than a stale-tab fallback")
	}
}

func TestServiceWorkerOpenRehydratesForeground(t *testing.T) {
	src := readServiceWorker(t)
	for _, want := range []string{
		"chrome.tabGroups.update(groupId, { collapsed: false })",
		"chrome.tabs.update(tab.id, { active: true })",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker open_tab must re-assert the opened tab as foreground; missing %q", want)
		}
	}
}

func TestBridgeOpenDefaultsToConfiguredGroup(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     400,
		groups:        map[int]*gaGroup{},
		tabs: []*gaTab{
			{id: 1, windowID: 1, groupID: -1, active: true, url: "https://x.test", title: "x"},
		},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	res, err := b.Open(ctx, "http://127.0.0.1:13333/foo")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fe.mu.Lock()
	gotGroup := fe.lastOpenGroupName
	fe.mu.Unlock()
	if gotGroup != "brw" {
		t.Fatalf("default Open landed in group %q, want \"brw\" (agent tabs must be corralled into the default group)", gotGroup)
	}
	if got := b.contextTabID(ctx); got != res.Tab.ID {
		t.Fatalf("after default-group Open, contextTabID=%q want opened tab %q", got, res.Tab.ID)
	}
}

func TestFocusTabThreadsRaiseWindowFlag(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     500,
		groups:        map[int]*gaGroup{},
		tabs: []*gaTab{
			{id: 1, windowID: 1, groupID: -1, active: true},
			{id: 2, windowID: 1, groupID: -1},
		},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	if err := b.FocusTab(ctx, "2"); err != nil {
		t.Fatalf("FocusTab: %v", err)
	}
	fe.mu.Lock()
	raised := fe.lastFocusRaiseWindow
	fe.mu.Unlock()
	if !raised {
		t.Fatal("library-default FocusTab should request raiseWindow=true (back-compat)")
	}

	b.SetRaiseWindowOnFocus(false)
	if err := b.FocusTab(ctx, "1"); err != nil {
		t.Fatalf("FocusTab: %v", err)
	}
	fe.mu.Lock()
	raised = fe.lastFocusRaiseWindow
	fe.mu.Unlock()
	if raised {
		t.Fatal("with SetRaiseWindowOnFocus(false), FocusTab must request raiseWindow=false so it never steals OS focus")
	}
}

func TestServiceWorkerFocusTabHonoursRaiseWindow(t *testing.T) {
	src := readServiceWorker(t)
	for _, want := range []string{
		"const raiseWindow = message.params?.raiseWindow === true;",
		"if (raiseWindow && before?.windowId) await chrome.windows.update(before.windowId, { focused: true });",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker focus_tab must gate the window-raise on raiseWindow; missing %q", want)
		}
	}
}
