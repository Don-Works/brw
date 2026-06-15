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

	"github.com/coder/websocket"
	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
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
		"ensureConnectAlarm();",
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
