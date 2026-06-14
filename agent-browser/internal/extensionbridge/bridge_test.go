package extensionbridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker reconnect/keepalive guard missing %q", want)
		}
	}
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
