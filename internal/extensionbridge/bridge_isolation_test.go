package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
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

func TestIsolationLostOwnedTabFailsClosedAndInvalidatesBeforeReuse(t *testing.T) {
	b := New("", 2*time.Second, "")
	b.SetFollowFocus(false)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial extension: %v", err)
	}
	defer conn.CloseNow()
	waitUntil(t, b.liveConn)

	cdpTargets := make(chan bool, 2)
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	go func() {
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var req request
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			reply := map[string]any{"id": req.ID, "ok": true, "result": map[string]any{}}
			if req.Type == "cdp" {
				_, pinned := req.Params["tabId"]
				cdpTargets <- pinned
				if pinned {
					reply["ok"] = false
					reply["error"] = "No tab with id: 42"
					delete(reply, "result")
				}
			}
			encoded, _ := json.Marshal(reply)
			_ = conn.Write(serveCtx, websocket.MessageText, encoded)
		}
	}()

	b.mu.Lock()
	b.active = "42"
	b.mu.Unlock()
	b.observeMu.Lock()
	b.observedState["42"] = &browser.SemanticState{URL: "https://closed.example.test/"}
	b.observeVersions["42"] = 4
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	b.downloadCursors["42"] = 6
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	b.emulationStates["42"] = bridgeDeviceEmulationState{HasBaseline: true}
	b.emulationMu.Unlock()

	_, err = b.cdp(context.Background(), "42", "Runtime.evaluate", map[string]any{"expression": "true"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no tab") {
		t.Fatalf("lost owned tab error = %v, want authoritative no-tab failure", err)
	}
	select {
	case pinned := <-cdpTargets:
		if !pinned {
			t.Fatal("first CDP request was not pinned to the owned tab")
		}
	default:
		t.Fatal("fake extension did not receive the pinned CDP request")
	}
	select {
	case <-cdpTargets:
		t.Fatal("isolation retried a lost owned-tab command without tabId, which could target the user's active tab")
	default:
	}

	b.mu.RLock()
	active := b.active
	b.mu.RUnlock()
	b.observeMu.Lock()
	_, observed := b.observedState["42"]
	_, versioned := b.observeVersions["42"]
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	_, cursor := b.downloadCursors["42"]
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	_, emulated := b.emulationStates["42"]
	b.emulationMu.Unlock()
	if active != "" || observed || versioned || cursor || emulated {
		t.Fatalf("authoritative no-tab failure retained reusable-id state: active=%q observed=%t versioned=%t cursor=%t emulated=%t", active, observed, versioned, cursor, emulated)
	}
}
