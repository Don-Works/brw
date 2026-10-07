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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

func TestExtTabToBrowserTabIncludesPopupMetadata(t *testing.T) {
	groupID := 9
	tab := extTab{
		ID:             42,
		URL:            "https://example.test/auth",
		Title:          "Authorize",
		Active:         true,
		Highlighted:    true,
		WindowID:       7,
		WindowFocused:  true,
		WindowType:     "popup",
		GroupID:        &groupID,
		GroupTitle:     "workspace-1",
		GroupColor:     "cyan",
		GroupCollapsed: true,
		GroupWarning:   "tab opened ungrouped: unavailable",
		OpenerTabID:    12,
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
	if tab.GroupID != "9" || tab.GroupTitle != "workspace-1" || tab.GroupColor != "cyan" || !tab.GroupCollapsed {
		t.Fatalf("missing group metadata: %+v", tab)
	}
	if tab.GroupWarning != "tab opened ungrouped: unavailable" {
		t.Fatalf("missing group warning: %+v", tab)
	}
}

func TestExtTabToBrowserTabTreatsUngroupedTabsAsDefault(t *testing.T) {
	none := -1
	tab := extTab{ID: 42, GroupID: &none}.toBrowserTab()
	if tab.GroupID != "" || tab.GroupTitle != "" || tab.GroupColor != "" || tab.GroupCollapsed {
		t.Fatalf("ungrouped tab should not expose group metadata: %+v", tab)
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

func TestBridgeObservationStateIsVersionedPerTab(t *testing.T) {
	b := New("", time.Second, "fake")
	one := browser.SemanticState{URL: "https://example.test", Signature: "one"}
	if version, changed := b.advanceObservation("41", one); version != 1 || !changed {
		t.Fatalf("first observation: version=%d changed=%v", version, changed)
	}
	if version, changed := b.advanceObservation("41", one); version != 1 || changed {
		t.Fatalf("unchanged observation: version=%d changed=%v", version, changed)
	}
	two := one
	two.Signature = "two"
	if version, changed := b.advanceObservation("41", two); version != 2 || !changed {
		t.Fatalf("changed observation: version=%d changed=%v", version, changed)
	}
	if version, changed := b.advanceObservation("42", one); version != 1 || !changed {
		t.Fatalf("second tab did not get independent state: version=%d changed=%v", version, changed)
	}
}

func TestBridgeTraceRecordsObservedActionsAndClears(t *testing.T) {
	b := New("", time.Second, "fake")
	result := browser.ActionResult{OK: true}
	b.finishObservedTrace(bridgeActionBaseline{Started: time.Now().Add(-25 * time.Millisecond)}, "clicked e7", &result)
	trace := b.GetTrace()
	if trace.Count != 1 || len(trace.Entries) != 1 {
		t.Fatalf("trace = %+v", trace)
	}
	entry := trace.Entries[0]
	if entry.Action != "click" || entry.Ref != "e7" || !entry.OK || entry.DurationMS < 20 || entry.Timestamp == "" {
		t.Fatalf("trace entry = %+v", entry)
	}
	if result.DurationMS < 20 {
		t.Fatalf("action result duration was not populated: %+v", result)
	}
	b.ClearTrace()
	if cleared := b.GetTrace(); cleared.Count != 0 || len(cleared.Entries) != 0 {
		t.Fatalf("cleared trace = %+v", cleared)
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

func TestBridgeStatusReportsIdentity(t *testing.T) {
	b := NewWithIdentity("", time.Second, "fake", brwidentity.Identity{
		Workspace:        "client-a",
		Profile:          "client-a-chrome",
		UserDataDir:      "/tmp/client-a",
		ProfileDirectory: "Profile 1",
		Mode:             "bridge",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	b.handleStatus(rec, req)

	var status struct {
		Identity brwidentity.Identity `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if status.Identity.Workspace != "client-a" || status.Identity.Profile != "client-a-chrome" || status.Identity.Mode != "bridge" {
		t.Fatalf("identity = %+v", status.Identity)
	}
}

func TestBatchAndPlanUseFastPrimitives(t *testing.T) {
	for _, funcName := range []string{"executeBatchStep", "executePlanStep"} {
		fn := findPackageFunc(t, ".", funcName)
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
				case "Click", "ClickText", "Type", "Fill", "Select", "Press", "Scroll", "Hover", "NavigateTo":
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

func stepActionCases(t *testing.T, path, funcName string) map[string]bool {
	t.Helper()
	fn := findPackageFunc(t, path, funcName)
	if fn == nil {
		t.Fatalf("missing %s in %s", funcName, path)
	}
	cases := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		selector, ok := stmt.Tag.(*ast.SelectorExpr)
		ident, identOK := selector.X.(*ast.Ident)
		if !ok || !identOK || ident.Name != "step" || selector.Sel.Name != "Action" {
			return true
		}
		for _, item := range stmt.Body.List {
			clause, ok := item.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					cases[strings.Trim(literal.Value, `"`)] = true
				}
			}
		}
		return false
	})
	return cases
}

func TestBatchAndPlanBackendsImplementTheSameActions(t *testing.T) {
	for _, tc := range []struct {
		runner   string
		funcName string
		required []string
	}{
		{runner: "batch", funcName: "executeBatchStep", required: []string{"navigate_to", "click_text", "assert", "find_act"}},
		{runner: "plan", funcName: "executePlanStep", required: []string{"navigate_to", "click_text", "find_act"}},
	} {
		t.Run(tc.runner, func(t *testing.T) {
			direct := stepActionCases(t, filepath.Join("..", "browser"), tc.funcName)
			bridge := stepActionCases(t, ".", tc.funcName)
			if !reflect.DeepEqual(direct, bridge) {
				t.Fatalf("%s action parity drift: direct-CDP=%v extension=%v", tc.runner, direct, bridge)
			}
			for _, action := range tc.required {
				if !bridge[action] {
					t.Fatalf("%s backends do not implement advertised action %q", tc.runner, action)
				}
			}
		})
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
		"BRIDGE_CONFIG_KEY",
		"BRW_CONFIGURE",
		"BRW_GET_STATUS",
		"assertDaemonIdentity",
		"globalThis.brwConfigure",
		"bridge-defaults.json",
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

	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
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
		`chrome.runtime.getURL("icons/icon-128.png")`,
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

func TestExtensionHasBridgeOptionsPage(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"options_page": "options.html"`) {
		t.Fatal("manifest must expose the bridge profile options page")
	}
	if !strings.Contains(string(manifest), `"default_popup": "popup.html"`) {
		t.Fatal("manifest must expose the toolbar status popup")
	}
	popupHTML, err := os.ReadFile(filepath.Join("..", "..", "extension", "popup.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="popup.css"`, `id="statusBlock"`, `id="reconnect"`, `id="openOptions"`, `id="detailsPanel"`, `aria-live="polite"`} {
		if !strings.Contains(string(popupHTML), want) {
			t.Fatalf("popup page missing %q", want)
		}
	}
	popupJS, err := os.ReadFile(filepath.Join("..", "..", "extension", "popup.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`BRW_GET_STATUS`, `BRW_RECONNECT`, `openOptionsPage`, `isVerifiedUp`, `LEXICON`, `Agent active`} {
		if !strings.Contains(string(popupJS), want) {
			t.Fatalf("popup script missing %q", want)
		}
	}
	worker, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`BADGE_DOWN_BG`,
		`BADGE_AGENT_BG`,
		`resolveBadgeMode`,
		`notifyBridgeDisconnected`,
		`BRW_RECONNECT`,
		`touchAgentActivity`,
		`brw · Idle`,
		`brw · Agent active`,
	} {
		if !strings.Contains(string(worker), want) {
			t.Fatalf("service worker missing badge/popup wiring %q", want)
		}
	}
	optionsHTML, err := os.ReadFile(filepath.Join("..", "..", "extension", "options.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="options.css"`, `id="statusBlock"`, `aria-live="polite"`, `id="advancedConfig"`, `id="rawStatus"`} {
		if !strings.Contains(string(optionsHTML), want) {
			t.Fatalf("options page missing %q", want)
		}
	}
	optionsCSS, err := os.ReadFile(filepath.Join("..", "..", "extension", "options.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prefers-color-scheme: dark", "prefers-reduced-motion: reduce", ":focus-visible"} {
		if !strings.Contains(string(optionsCSS), want) {
			t.Fatalf("options styles missing %q", want)
		}
	}
	popupCSS, err := os.ReadFile(filepath.Join("..", "..", "extension", "popup.css"))
	if err != nil {
		t.Fatal(err)
	}

	for _, surface := range []struct {
		name string
		css  []byte
	}{{"options.css", optionsCSS}, {"popup.css", popupCSS}} {
		heights := controlMinHeights(string(surface.css))
		if len(heights) == 0 {
			t.Fatalf("%s declares no min-height on .button or input; the hit-target floor is unenforced", surface.name)
		}
		for selector, px := range heights {
			if px < 44 {
				t.Fatalf("%s: %s has min-height %gpx, below the 44px hit-target floor", surface.name, selector, px)
			}
		}
	}
	optionsJS, err := os.ReadFile(filepath.Join("..", "..", "extension", "options.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`BRW_CONFIGURE`, `BRW_GET_STATUS`, `ws://127.0.0.1:${port}/extension`, "validateEndpoints", "setInterval", "actionableFailure"} {
		if !strings.Contains(string(optionsJS), want) {
			t.Fatalf("options page missing %q", want)
		}
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

	b := New("", time.Second, "")
	if err := b.FocusTab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "tab id is required") {
		t.Fatalf("FocusTab(\"\") error = %v, want 'tab id is required'", err)
	}
	if err := b.CloseTab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "tab id is required") {
		t.Fatalf("CloseTab(\"\") error = %v, want 'tab id is required'", err)
	}
}

func TestCloseTabClearsEmulationStateBeforeNumericIDReuse(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := connectRetargetFake(t, b, 42)
	defer cleanup()

	b.emulationMu.Lock()
	b.emulationStates["42"] = bridgeDeviceEmulationState{
		HasBaseline: true,
		Baseline:    bridgeDeviceIdentity{UserAgent: "closed-tab-agent", Platform: "closed-tab-platform"},
	}
	b.emulationMu.Unlock()

	if err := b.CloseTab(context.Background(), "42"); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	identity, found, err := b.deviceEmulationBaseline(context.Background(), "42", false)
	if err != nil {
		t.Fatalf("baseline lookup after close: %v", err)
	}
	if found || identity != (bridgeDeviceIdentity{}) {
		t.Fatalf("reused tab id inherited closed-tab emulation baseline: found=%t identity=%+v", found, identity)
	}
}

func TestOpenInGroupRejectsInvalidGroupIDBeforeBridgeCall(t *testing.T) {
	b := New("", time.Second, "")
	_, err := b.OpenInGroup(context.Background(), "https://example.com", browser.TabGroupOptions{GroupID: "not-a-number"})
	if err == nil || !strings.Contains(err.Error(), "invalid group id") {
		t.Fatalf("OpenInGroup invalid group id error = %v, want invalid group id", err)
	}
}

func TestContextTabIDQueriesLiveActiveTab(t *testing.T) {

	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
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

	b.setActiveTabID("11")

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

	b := New("", time.Second, "")
	ctx := browser.WithTabID(context.Background(), "tab-9")
	if got := b.contextTabID(ctx); got != "tab-9" {
		t.Fatalf("contextTabID with explicit tab = %q, want tab-9", got)
	}
}

func TestBridgeConditionSupportsCommitted(t *testing.T) {

	src := packageSource(t, ".")
	if !strings.Contains(snapshot.WaitConditionScript, `condition==='committed'`) {
		t.Fatal("shared wait script must handle the 'committed' condition")
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

func TestServiceWorkerInvalidatesSnapshotCacheOnSPARouteChange(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "chrome.webNavigation.onHistoryStateUpdated.addListener") {
		t.Fatal("service worker must listen on onHistoryStateUpdated to invalidate cache on SPA pushState navigations")
	}

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

	if !strings.Contains(src, "chrome.tabs.get(tab.id)") {
		t.Fatal("listTabSummaries must refresh each tab via chrome.tabs.get for fresh url/title")
	}

	if !strings.Contains(src, `message.type === "get_active_tab_id"`) {
		t.Fatal("service worker must handle get_active_tab_id for live active-tab resolution")
	}
}

func TestServiceWorkerExposesTabGroupControls(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		`message.type === "list_tab_groups"`,
		"async function listTabGroups()",
		"groupId: explicitGroupId",
		"groupTitle: group?.title",
		"groupCollapsed: Boolean(group?.collapsed)",
		"await findGroupByTitle(groupName, tab.windowId)",
		"preferredNormalWindowId()",
		"tab opened ungrouped:",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker tab-group support missing %q", want)
		}
	}
}

func TestServiceWorkerUsesNativePointerAndConsoleEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		`message.type === "move_pointer"`,
		`"Input.dispatchMouseEvent"`,
		`forceHoverAt(tabId, x, y)`,
		`"CSS.forcePseudoState"`,
		`message.type === "get_console_messages"`,
		`message.type === "get_tab_input_state"`,
		`message.type === "capture_screenshot"`,
		`captureScreenshotForTab(tabId, params)`,
		`"Page.captureScreenshot"`,
		`"Page.printToPDF"`,
		`fallback: "pdf"`,
		`forceDetach(tabId)`,
		`method === "Runtime.consoleAPICalled"`,
		`method === "Runtime.exceptionThrown"`,
		`"Runtime.enable"`,
		`"Emulation.setFocusEmulationEnabled"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker native input/console support missing %q", want)
		}
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

	const wantManifest = "0.7.12"
	if m.Version != wantManifest {
		t.Fatalf("manifest version = %q, want %q", m.Version, wantManifest)
	}

	worker, err := os.ReadFile(filepath.Join("..", "..", "extension", "service_worker.js"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(worker), `const PROTOCOL_VERSION = "0.2.0";`) {
		t.Fatal("service worker PROTOCOL_VERSION must remain 0.2.0 (no bridge-handshake change in this release)")
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

func packageFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		if !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
	}
	return out
}

func findPackageFunc(t *testing.T, dir, name string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	for _, p := range packageFiles(t, dir) {
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if fn := findFunc(file, name); fn != nil {
			return fn
		}
	}
	return nil
}

func packageSource(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range packageFiles(t, dir) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
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

func controlMinHeights(css string) map[string]float64 {
	out := map[string]float64{}
	for _, block := range strings.Split(css, "}") {
		open := strings.Index(block, "{")
		if open < 0 {
			continue
		}
		selector := strings.TrimSpace(block[:open])
		if i := strings.LastIndex(selector, "\n"); i >= 0 {
			selector = strings.TrimSpace(selector[i+1:])
		}
		if !stylesAControl(selector) {
			continue
		}
		for _, decl := range strings.Split(block[open+1:], ";") {
			name, value, ok := strings.Cut(decl, ":")
			if !ok || strings.TrimSpace(name) != "min-height" {
				continue
			}
			value = strings.TrimSpace(value)
			if !strings.HasSuffix(value, "px") {
				continue
			}
			px, err := strconv.ParseFloat(strings.TrimSuffix(value, "px"), 64)
			if err != nil {
				continue
			}
			out[selector] = px
		}
	}
	return out
}

func stylesAControl(selector string) bool {
	switch selector {
	case ".button", "input", "button", "select", "textarea",
		`input[type="text"]`, `input[type="url"]`:
		return true
	}
	return false
}

func TestControlMinHeightsReadsTheProperty(t *testing.T) {
	got := controlMinHeights(`
/* min-height: 44px in a comment must not count */
.button {
  padding: 0 14px;
  min-height: 44px;
}
.button.primary {
  font-weight: 600;
}
input {
  min-height: 34px;
}
.shell {
  min-height: 100vh;
}
textarea {
  min-height: 60em;
}
`)
	want := map[string]float64{".button": 44, "input": 34}
	if len(got) != len(want) {
		t.Fatalf("controlMinHeights() = %v, want %v", got, want)
	}
	for selector, px := range want {
		if got[selector] != px {
			t.Fatalf("controlMinHeights()[%q] = %v, want %v", selector, got[selector], px)
		}
	}
}
