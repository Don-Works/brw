package extensionbridge

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// newIsolationExtension builds a group-aware fake with a single tab the USER has
// focused — the tab a worker must NOT stomp.
func newIsolationExtension(nextTabID int) *groupAwareExtension {
	return &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     nextTabID,
		groups:        map[int]*gaGroup{},
		tabs: []*gaTab{
			{id: 1, windowID: 1, groupID: -1, active: true, url: "https://users-page.test", title: "User's work"},
		},
	}
}

// TestIsolationAutoOpensOwnTabInsteadOfUsersFocusedTab is the core regression for
// the reported "stomping all over existing tabs" bug. In isolation (the daemon
// default), the first no-tab_id resolution — what the MCP/HTTP entry runs before
// every page tool — must open a fresh tab in the default group rather than
// resolving the user's focused tab, and it must open in the BACKGROUND so the
// user's current tab stays put.
func TestIsolationAutoOpensOwnTabInsteadOfUsersFocusedTab(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	b.SetFollowFocus(false)
	fe := newIsolationExtension(300)
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	got := b.ResolveActiveTabID(ctx)
	if got == "" || got == "1" {
		t.Fatalf("isolation ResolveActiveTabID = %q; it must open a NEW tab, not reuse the user's focused tab 1", got)
	}

	fe.mu.Lock()
	group := fe.lastOpenGroupName
	background := fe.lastOpenBackground
	userTabActive := fe.tabByID(1).active
	fe.mu.Unlock()

	if group != "brw" {
		t.Fatalf("auto-open landed in group %q, want \"brw\" (the worker's own group)", group)
	}
	if !background {
		t.Fatal("auto-open must open in the background (active:false) so it never switches the tab the user is on")
	}
	if !userTabActive {
		t.Fatal("the user's focused tab (1) must stay active after a background auto-open")
	}

	// A subsequent no-tab_id resolution stays on the worker's owned tab.
	if again := b.contextTabID(ctx); again != got {
		t.Fatalf("second no-tab_id resolution = %q, want the owned tab %q (it must not drift back to the user's tab)", again, got)
	}
}

// TestIsolationDoesNotChaseUserTabSwitch proves that once a worker owns a tab,
// the user manually switching to another tab does NOT pull the worker onto it —
// the opposite of the legacy follow-focus behavior.
func TestIsolationDoesNotChaseUserTabSwitch(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	b.SetFollowFocus(false)
	fe := newIsolationExtension(300)
	// Add a second user tab to switch to.
	fe.tabs = append(fe.tabs, &gaTab{id: 2, windowID: 1, groupID: -1, active: false, url: "https://other.test", title: "Other"})
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	owned := b.ResolveActiveTabID(ctx)
	if owned == "" || owned == "1" || owned == "2" {
		t.Fatalf("expected a freshly opened owned tab, got %q", owned)
	}

	// The user switches focus to tab 2 (and the extension would push active_tab).
	fe.mu.Lock()
	fe.activateExclusive(1, 2)
	fe.mu.Unlock()

	if got := b.contextTabID(ctx); got != owned {
		t.Fatalf("after the user switched to tab 2, isolation resolved %q, want the worker's owned tab %q", got, owned)
	}
}

// TestIsolationExplicitTabIDWins proves that targeting an existing tab is still
// possible: an explicit tab_id always resolves to that tab and never triggers an
// auto-open ("unless we're specifically working with an existing tab").
func TestIsolationExplicitTabIDWins(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	b.SetFollowFocus(false)
	fe := newIsolationExtension(300)
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "42")
	if got := b.ResolveActiveTabID(ctx); got != "42" {
		t.Fatalf("explicit tab_id must win in isolation; got %q want 42", got)
	}

	fe.mu.Lock()
	next := fe.nextTabID
	fe.mu.Unlock()
	if next != 300 {
		t.Fatalf("explicit tab_id must NOT trigger an auto-open; nextTabID advanced to %d", next)
	}
}

// TestIsolationAutoOpenCooldownPreventsCascade is the regression for the reported
// "brw_evaluate 20003ms x 35 calls" spike: when a browser is wedged and open_tab
// never succeeds, the isolation auto-open must NOT be re-attempted on every
// no-tab_id call (each paying the full timeout). A cooldown limits it to one
// attempt per window.
func TestIsolationAutoOpenCooldownPreventsCascade(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	b.SetFollowFocus(false)
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     700,
		groups:        map[int]*gaGroup{},
		failOpen:      true, // model a wedged extension: open_tab always fails
		tabs:          []*gaTab{{id: 1, windowID: 1, groupID: -1, active: true, url: "https://user.test"}},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	// Several back-to-back no-tab_id resolutions (what a worker's rapid calls do).
	for i := 0; i < 5; i++ {
		b.ResolveActiveTabID(ctx)
	}

	fe.mu.Lock()
	attempts := fe.openCalls
	fe.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("open_tab attempted %d times across 5 no-tab_id resolves; the cooldown must limit a wedged browser to 1 attempt per window (else every call cascades to the full timeout)", attempts)
	}
}

// TestFollowFocusModeResolvesUsersTabWithoutOpening proves the escape hatch:
// --bridge-follow-focus (SetFollowFocus(true)) restores the legacy behavior where
// a no-tab_id action acts on the user's focused tab and never auto-opens.
func TestFollowFocusModeResolvesUsersTabWithoutOpening(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	b.SetFollowFocus(true)
	fe := newIsolationExtension(300)
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	ctx := context.Background()
	if got := b.ResolveActiveTabID(ctx); got != "1" {
		t.Fatalf("follow-focus ResolveActiveTabID = %q, want the user's focused tab 1", got)
	}

	fe.mu.Lock()
	next := fe.nextTabID
	fe.mu.Unlock()
	if next != 300 {
		t.Fatalf("follow-focus must NOT auto-open; nextTabID advanced to %d", next)
	}
}
