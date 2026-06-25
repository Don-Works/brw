package browser

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// TestExternallyClosedTabIsReclaimed proves the targetDestroyed listener cleans
// up per-tab state when a tab is closed OUTSIDE brw_close_tab (the user clicking
// the X, window.close(), a crash). Without the listener these contexts, the
// chromedp target-handler goroutine, and the ref store entry leak for the life
// of the daemon.
func TestExternallyClosedTabIsReclaimed(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := res.Tab.ID
	if id == "" {
		t.Fatal("expected a tab id")
	}

	// Force the per-tab context to be created (Open is lazy) and record refs, so
	// there is real state to leak.
	if _, err := m.tabContext(id); err != nil {
		t.Fatalf("tabContext: %v", err)
	}
	m.mu.RLock()
	_, tracked := m.tabContexts[id]
	m.mu.RUnlock()
	if !tracked {
		t.Fatal("tab context was not created")
	}

	// Close the target directly via CDP — NOT via CloseTab — to simulate an
	// external close. CloseTab would do its own cleanup and mask the listener.
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.CloseTarget(target.ID(id)).Do(ctx)
	}); err != nil {
		t.Fatalf("CloseTarget: %v", err)
	}

	// The targetDestroyed event is async; poll until the listener reclaims state.
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.RLock()
		_, stillTracked := m.tabContexts[id]
		m.mu.RUnlock()
		if !stillTracked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tab context still tracked 10s after external close — targetDestroyed listener did not reclaim it (leak)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
