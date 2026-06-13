package extensionbridge

import (
	"testing"

	"github.com/revitt/agent-browser/internal/browser"
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
