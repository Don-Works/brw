package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

func TestExtTabToBrowserTabIncludesPopupMetadata(t *testing.T) {
	tab := extTab{
		ID:            42,
		URL:           "https://example.test/auth",
		Title:         "Authorize",
		Active:        true,
		Highlighted:   true,
		WindowID:      7,
		WindowFocused: true,
		WindowType:    "popup",
		OpenerTabID:   12,
	}.toBrowserTab()

	if tab.ID != "42" || tab.Type != "popup" || !tab.Popup {
		t.Fatalf("unexpected popup mapping: %+v", tab)
	}
	if tab.WindowID != 7 || !tab.WindowFocused || !tab.Active || !tab.Highlighted {
		t.Fatalf("missing window/focus metadata: %+v", tab)
	}
	if tab.OpenerTabID != "12" {
		t.Fatalf("missing opener id: %+v", tab)
	}
}

func TestActionTargetsPrioritizesActiveThenPopups(t *testing.T) {
	tabs := []browser.Tab{
		{ID: "1", URL: "https://main.test", Type: "page", Active: true},
		{ID: "2", URL: "https://auth.test", Type: "popup", Popup: true, Active: true, WindowFocused: true},
		{ID: "3", URL: "https://other.test", Type: "page"},
	}

	targets := actionTargets(tabs, "1", 8)
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(targets), targets)
	}
	if targets[0].ID != "1" || targets[1].ID != "2" {
		t.Fatalf("unexpected target order: %+v", targets)
	}
}

func TestSnapshotCacheKeyDistinguishesOptions(t *testing.T) {
	base := snapshot.SnapshotOptions{Limit: 12, ViewportOnly: true}
	if snapshotCacheKey(base) == snapshotCacheKey(snapshot.SnapshotOptions{Limit: 12, ViewportOnly: true, Role: "searchbox"}) {
		t.Fatal("snapshot cache key must include role filters")
	}
	if snapshotCacheKey(base) == snapshotCacheKey(snapshot.SnapshotOptions{Limit: 12, ViewportOnly: true, Query: "running"}) {
		t.Fatal("snapshot cache key must include query filters")
	}
	if snapshotCacheKey(snapshot.SnapshotOptions{Limit: 1, IncludeAX: true}) != snapshotCacheKey(snapshot.SnapshotOptions{Limit: 1, IncludeAX: false}) {
		t.Fatal("extension bridge cache key should ignore IncludeAX because AX is disabled on the bridge")
	}
}

func TestBridgeStatusReportsConnectionLifecycle(t *testing.T) {
	b := New("", time.Second, "fake")
	now := time.Date(2026, 6, 17, 22, 0, 0, 123, time.UTC)
	ch := make(chan response, 1)
	b.mu.Lock()
	b.conn = &websocket.Conn{}
	b.connectedAt = now
	b.lastSeenAt = now.Add(time.Second)
	b.disconnectedAt = now.Add(2 * time.Second)
	b.disconnectReason = "read failed"
	b.pending["42"] = ch
	b.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	b.handleStatus(rec, req)

	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if status["connected"] != true {
		t.Fatalf("connected = %v, want true", status["connected"])
	}
	if status["connected_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("connected_at = %v", status["connected_at"])
	}
	if status["last_seen_at"] != now.Add(time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("last_seen_at = %v", status["last_seen_at"])
	}
	if status["disconnected_at"] != now.Add(2*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("disconnected_at = %v", status["disconnected_at"])
	}
	if status["disconnect_reason"] != "read failed" {
		t.Fatalf("disconnect_reason = %v", status["disconnect_reason"])
	}
	if status["pending"] != float64(1) {
		t.Fatalf("pending = %v, want 1", status["pending"])
	}
}

func TestBatchAndPlanUseFastPrimitives(t *testing.T) {
	srcPath := filepath.Join("bridge.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, funcName := range []string{"executeBatchStep", "executePlanStep"} {
		fn := findFunc(file, funcName)
		if fn == nil {
			t.Fatalf("missing %s", funcName)
		}
		var calls []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if ok && ident.Name == "b" {
				switch sel.Sel.Name {
				case "Click", "Type", "Fill", "Select", "Press", "Scroll", "Hover":
					calls = append(calls, sel.Sel.Name)
				}
			}
			return true
		})
		if len(calls) > 0 {
			t.Fatalf("%s must use raw primitives, not observed wrappers: %v", funcName, calls)
		}
	}
}

func TestServiceWorkerReconnectCadence(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		"periodInMinutes: 0.5",
		"5 * 1000",
		"BRIDGE_STATUS_URL",
		"DAEMON_STATUS_INTERVAL_MS",
		"ensureConnectAlarm();",
		"SW_KEEPALIVE",
		"offscreenSetupPromise",
		"connectPromise",
		"probeDaemonStatus",
		"setBridgeBadge",
		"send failed",
		"chrome.offscreen.Reason",
		"sendDebuggerCommand(tabId",
		"isDetachedDebuggerError",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker reconnect/keepalive guard missing %q", want)
		}
	}
}

func TestBridgeDebuggerDetachedErrors(t *testing.T) {
	for _, msg := range []string{
		"Detached while handling command.",
		"Debugger is not attached to the tab with id: 123",
		"target closed",
	} {
		if !isBridgeDebuggerDetachedError(errors.New(msg)) {
			t.Fatalf("expected debugger detach retry error for %q", msg)
		}
	}
	if isBridgeDebuggerDetachedError(errors.New("ref not found")) {
		t.Fatal("semantic/action errors must not be treated as debugger detach retries")
	}
}

func TestBridgeNotifyEmitsNotifyCommandAndRoundTrips(t *testing.T) {
	b := New("", 5*time.Second, "")

	// Serve the bridge's /extension websocket endpoint over a test server so a
	// fake extension client can connect without binding a fixed port.
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Wait for the bridge to register the connection before issuing a command.
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	type notifyOut struct {
		result browser.NotifyResult
		err    error
	}
	done := make(chan notifyOut, 1)
	go func() {
		result, err := b.Notify(context.Background(), browser.NotifyOptions{
			Kind:    "needs_input",
			Title:   "MFA required",
			Message: "Enter your one-time code",
		})
		done <- notifyOut{result, err}
	}()

	// Act as the extension: read the emitted bridge command and assert payload.
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read bridge command: %v", err)
	}
	var cmd struct {
		ID     string         `json:"id"`
		Type   string         `json:"type"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		t.Fatalf("unmarshal bridge command: %v", err)
	}
	if cmd.Type != "notify" {
		t.Fatalf("bridge command type = %q, want notify", cmd.Type)
	}
	if cmd.Params["kind"] != "needs_input" || cmd.Params["title"] != "MFA required" || cmd.Params["message"] != "Enter your one-time code" {
		t.Fatalf("bridge notify params = %#v", cmd.Params)
	}

	// Reply as the extension would after chrome.notifications.create succeeds.
	reply, _ := json.Marshal(map[string]any{
		"id": cmd.ID,
		"ok": true,
		"result": map[string]any{
			"ok":       true,
			"delivery": "extension",
			"note":     "notif-id-1",
		},
	})
	if err := conn.Write(readCtx, websocket.MessageText, reply); err != nil {
		t.Fatalf("write reply: %v", err)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Notify returned error: %v", out.err)
		}
		if !out.result.OK || out.result.Delivery != "extension" || out.result.Note != "notif-id-1" {
			t.Fatalf("notify result = %#v", out.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify did not return after extension reply")
	}
}

func TestServiceWorkerHandlesNotifyCommand(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		`message.type === "notify"`,
		"createNotification(",
		"chrome.notifications.create",
		`delivery: "extension"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker notify handler missing %q", want)
		}
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"notifications"`) {
		t.Fatal("manifest must request the notifications permission")
	}
}

func TestRequireTabIDRejectsEmptyAndInvalid(t *testing.T) {
	for _, bad := range []string{"", "   ", "0", "-5", "abc", "12x"} {
		if _, err := requireTabID(bad); err == nil {
			t.Fatalf("requireTabID(%q) = nil error, want error", bad)
		}
	}
	got, err := requireTabID(" 42 ")
	if err != nil {
		t.Fatalf("requireTabID(\" 42 \") error = %v, want nil", err)
	}
	if got != 42 {
		t.Fatalf("requireTabID(\" 42 \") = %d, want 42", got)
	}
}

func TestFocusAndCloseTabRejectEmptyIDBeforeBridgeCall(t *testing.T) {
	// No extension connected: a valid id would fail with "not connected", but an
	// empty id must fail earlier with the clear validation error so a batched
	// script does not produce the opaque "No tab with id: 0".
	b := New("", time.Second, "")
	if err := b.FocusTab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "tab id is required") {
		t.Fatalf("FocusTab(\"\") error = %v, want 'tab id is required'", err)
	}
	if err := b.CloseTab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "tab id is required") {
		t.Fatalf("CloseTab(\"\") error = %v, want 'tab id is required'", err)
	}
}

func TestContextTabIDQueriesLiveActiveTab(t *testing.T) {
	// The daemon's cached active tab (b.active) drifts when the user switches
	// tabs manually. contextTabID must ask the extension for the genuinely active
	// tab via get_active_tab_id rather than blindly returning the stale cache.
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	// Seed a stale cached value the way an old FocusTab would have.
	b.setActiveTabID("11")

	// Fake extension: reply to get_active_tab_id with the truly focused tab 77.
	go func() {
		ctx := context.Background()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &cmd)
		if cmd.Type != "get_active_tab_id" {
			return
		}
		reply, _ := json.Marshal(map[string]any{
			"id":     cmd.ID,
			"ok":     true,
			"result": map[string]any{"tabId": 77},
		})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}()

	got := b.contextTabID(context.Background())
	if got != "77" {
		t.Fatalf("contextTabID = %q, want 77 (live active tab, not stale cache)", got)
	}
	if b.activeTabID() != "77" {
		t.Fatalf("cached active tab = %q, want healed to 77", b.activeTabID())
	}
}

func TestContextTabIDPrefersExplicitContextTab(t *testing.T) {
	// An explicit tab id in the context must win without any extension round-trip.
	b := New("", time.Second, "")
	ctx := browser.WithTabID(context.Background(), "tab-9")
	if got := b.contextTabID(ctx); got != "tab-9" {
		t.Fatalf("contextTabID with explicit tab = %q, want tab-9", got)
	}
}

func TestBridgeConditionSupportsCommitted(t *testing.T) {
	// The committed condition (used by Open for non-blank URLs) must be present
	// in the bridge's in-page condition script so it is not silently ignored.
	srcPath := filepath.Join("bridge.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, `condition === "committed"`) {
		t.Fatal("bridge condition() must handle the 'committed' condition")
	}
	if !strings.Contains(src, `b.WaitFor(waitCtx, "committed"`) {
		t.Fatal("bridge Open() must wait for 'committed' on non-blank URLs")
	}
	if !strings.Contains(src, `b.WaitFor(waitCtx, "ready"`) {
		t.Fatal("bridge Open() must wait for 'ready' on about:blank")
	}
}

func TestServiceWorkerInvalidatesSnapshotCacheOnNavigation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		"chrome.webNavigation.onCommitted.addListener",
		"details.frameId === 0",
		"state.snapshotCache.delete(details.tabId)",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker navigation cache invalidation missing %q", want)
		}
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"webNavigation"`) {
		t.Fatal("manifest must request the webNavigation permission for onCommitted")
	}
}

// TestServiceWorkerInvalidatesSnapshotCacheOnSPARouteChange guards the SPA fix:
// client-side pushState/replaceState navigations do not fire onCommitted, so the
// service worker must ALSO listen on onHistoryStateUpdated (main frame) and drop
// the per-tab snapshot cache, otherwise an SPA route change serves stale content.
func TestServiceWorkerInvalidatesSnapshotCacheOnSPARouteChange(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "chrome.webNavigation.onHistoryStateUpdated.addListener") {
		t.Fatal("service worker must listen on onHistoryStateUpdated to invalidate cache on SPA pushState navigations")
	}
	// The handler must scope to the main frame and bust the snapshot cache.
	idx := strings.Index(src, "onHistoryStateUpdated.addListener")
	if idx < 0 {
		t.Fatal("onHistoryStateUpdated listener not found")
	}
	handler := src[idx:]
	end := strings.Index(handler, "});")
	if end > 0 {
		handler = handler[:end]
	}
	for _, want := range []string{"details.frameId === 0", "state.snapshotCache.delete(details.tabId)"} {
		if !strings.Contains(handler, want) {
			t.Fatalf("onHistoryStateUpdated handler missing %q", want)
		}
	}
}

func TestServiceWorkerRefreshesListTabsAndExposesActiveTabQuery(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	// list_tabs must re-fetch each tab to avoid stale populated url/title.
	if !strings.Contains(src, "chrome.tabs.get(tab.id)") {
		t.Fatal("listTabSummaries must refresh each tab via chrome.tabs.get for fresh url/title")
	}
	// get_active_tab_id handler backs the live active-tab query.
	if !strings.Contains(src, `message.type === "get_active_tab_id"`) {
		t.Fatal("service worker must handle get_active_tab_id for live active-tab resolution")
	}
}

func TestExtensionReleaseVersion(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	const want = "0.0.1"
	if m.Version != want {
		t.Fatalf("manifest version = %q, want %q", m.Version, want)
	}

	worker, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), `const PROTOCOL_VERSION = "0.0.1";`) {
		t.Fatal("service worker protocol version must match the 0.0.1 extension release")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
