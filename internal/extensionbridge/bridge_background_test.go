package extensionbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

func TestBackgroundTabOpenKeepsDefaultAndPinsCreatingConnection(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetFollowFocus(false)
	f := newIsolationExtension(700)
	cleanup := connectGroupAwareExtension(t, b, f)
	defer cleanup()
	b.setActiveTabID("1")
	ctx := browser.WithBackgroundPage(context.Background())
	opened, err := b.Open(ctx, "https://chat.test/")
	if err != nil {
		t.Fatal(err)
	}
	if b.activeTabID() != "1" {
		t.Fatal("background open changed default tab")
	}
	f.mu.Lock()
	background := f.lastOpenBackground
	f.mu.Unlock()
	if !background {
		t.Fatal("watcher open foregrounded private tab")
	}
	if err := b.CheckBackgroundTab(ctx, opened.Tab.ID); err != nil {
		t.Fatalf("fresh ownership not pinned: %v", err)
	}
	b.ReleaseBackgroundTab(opened.Tab.ID)
	if err := b.CheckBackgroundTab(ctx, opened.Tab.ID); !errors.Is(err, browser.ErrBackgroundOwnershipLost) {
		t.Fatalf("released ownership survived: %v", err)
	}
}

func TestBackgroundCallsCannotBypassReconnectedSocketOwnership(t *testing.T) {
	b := New("", time.Second, "")
	first, second := &websocket.Conn{}, &websocket.Conn{}
	b.conn = first
	b.backgroundTabs = map[string]*websocket.Conn{"42": first}
	ctx := browser.WithBackgroundPage(browser.WithTabID(context.Background(), "42"))
	if err := b.CheckBackgroundTab(ctx, "42"); err != nil {
		t.Fatal(err)
	}
	b.conn = second
	if err := b.CheckBackgroundTab(ctx, "42"); !errors.Is(err, browser.ErrBackgroundOwnershipLost) {
		t.Fatalf("recycled id accepted: %v", err)
	}
	for _, call := range []struct {
		typ    string
		params map[string]any
	}{{"cdp", map[string]any{"tabId": 42, "method": "Runtime.evaluate"}}, {"cdp", map[string]any{"tabId": 42, "method": "Page.reload"}}, {"close_tab", map[string]any{"tabId": 42}}} {
		if _, err := b.dispatch(ctx, call.typ, call.params); !errors.Is(err, browser.ErrBackgroundOwnershipLost) {
			t.Fatalf("stale background operation was dispatched: %v", err)
		}
	}
}
