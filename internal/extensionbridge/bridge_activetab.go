package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// contextTabID resolves the tab a page action targets.
//
// Latency profile: when no explicit tab_id is in the context, this makes one
// synchronous get_active_tab_id RPC to the extension per call (every Snapshot,
// Read, Click, etc.). That is a deliberate correctness-over-latency trade: the
// cached b.active reference drifts when the user switches tabs manually in
// Chrome, and acting on the wrong tab is worse than a sub-millisecond local-WS
// round-trip. Callers issuing rapid-fire actions should pass an explicit tab_id
// (which skips the query entirely) or use brw_batch / brw_plan, which
// resolve the tab once for the whole sequence.
func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if !b.followFocus {
		// Isolation: target the tab brw OWNS. b.active is set only by brw's own
		// Open/FocusTab (never by user-focus signals — those writes are guarded),
		// so this never chases the user's manual tab switches. Returns "" when brw
		// owns no tab yet; the top-level entry (ensureOwnedTabID) opens one then.
		return b.activeTabID()
	}
	// No explicit tab in context: resolve the browser's genuinely focused tab
	// from the extension rather than trusting the cached b.active reference,
	// which drifts when the user switches tabs manually in Chrome (the daemon
	// only updates b.active on explicit FocusTab/ListTabs/Open).
	//
	// Retry briefly before trusting the cache. The MV3 service worker sleeps when
	// Chrome is idle and may be mid-reconnect when a tool call arrives; a single
	// transient resolution failure must NOT silently drop us onto a stale cached
	// tab, because that is exactly how read/observe/snapshot end up resolving three
	// different tabs (one re-resolves live, another serves the stale cache). Three
	// quick attempts ride out a reconnect hiccup; only a genuinely unreachable
	// extension falls through to the last-known tab.
	for attempt := 0; attempt < activeTabResolveAttempts; attempt++ {
		live, reason := b.resolveActiveTabIDWithReason(ctx)
		if live != "" {
			return live
		}
		// A definitive "nothing here is drivable" is not a reconnect hiccup.
		// Retrying it and then falling through to b.activeTabID() re-targets the
		// stale cached tab — which is typically the very tab that is undrivable —
		// so the caller kept getting Chrome's raw refusal instead of the real
		// reason. Return "" instead: the extension re-resolves on the far side and
		// the call fails with the actionable message.
		if isNoDrivableTabReason(reason) {
			return ""
		}
		if attempt < activeTabResolveAttempts-1 {
			select {
			case <-ctx.Done():
				return b.activeTabID()
			case <-time.After(activeTabResolveBackoff):
			}
		}
	}
	return b.activeTabID()
}

// ensureOwnedTabID resolves the tab a top-level no-tab_id tool call must target,
// opening one if necessary. An explicit tab_id in ctx always wins (the agent
// chose to work with that existing tab). In isolation (the daemon default) it
// returns the tab brw owns, opening a fresh tab in the default group when brw
// owns none yet — so a worker's first action lands on its own new tab instead of
// the user's focused tab. In follow-focus mode it defers to the live resolver.
func (b *Bridge) ensureOwnedTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if b.followFocus {
		return b.contextTabID(ctx)
	}
	owned, reconciled := b.reconciledOwnedTab()
	if !reconciled {
		// Do not trust ownership cached from the displaced worker while the socket
		// is down. getConn parks through the short MV3 gap; handleExtension
		// reconciles agent_tab_id before waking it.
		if _, err := b.getConn(ctx); err != nil {
			return ""
		}
		owned, reconciled = b.reconciledOwnedTab()
		if !reconciled {
			return ""
		}
	}
	if owned != "" {
		return owned
	}
	// No owned tab yet: open one (default group, background) so the call never
	// acts on the user's existing tab. Two guards keep a wedged browser from
	// turning one slow open into a per-call 20s hang (the brw_evaluate cascade):
	//   1. cooldown — after a recent failure, fast-fail instead of re-opening.
	//   2. bounded timeout — a single attempt cannot exceed autoOpenTimeout.
	b.mu.RLock()
	failedAt := b.autoOpenFailedAt
	b.mu.RUnlock()
	if !failedAt.IsZero() && time.Since(failedAt) < autoOpenCooldown {
		return b.activeTabID()
	}

	openCtx, cancel := context.WithTimeout(ctx, autoOpenTimeout)
	defer cancel()
	res, err := b.Open(openCtx, isolationSeedURL)
	if err != nil || res.Tab.ID == "" {
		b.mu.Lock()
		b.autoOpenFailedAt = time.Now()
		b.mu.Unlock()
		log.Printf("brw: isolation auto-open failed (%v); no-tab_id actions fast-fail for %s — reload the brw extension in chrome://extensions, or pass an explicit tab_id", err, autoOpenCooldown)
		return b.activeTabID()
	}
	// Success clears any prior failure so normal isolation resumes immediately.
	b.mu.Lock()
	b.autoOpenFailedAt = time.Time{}
	b.mu.Unlock()
	return res.Tab.ID
}

// reconciledOwnedTab returns an isolation pin only when it belongs to the
// currently published connection generation. A disconnected bridge may retain
// b.active for a same-worker reconnect, but callers must wait for the new hello
// to confirm it before using that numeric id.
func (b *Bridge) reconciledOwnedTab() (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active, b.conn != nil && b.agentPinKnown && !b.shuttingDown
}

// ResolveActiveTabID resolves the tab a top-level no-tab_id tool call targets and
// returns it (or "" when it cannot be determined / opened). The MCP / HTTP entry
// points call this when no explicit tab_id is supplied and pin the result into the
// request context via browser.WithCurrentOwnedTabID, so every downstream contextTabID()
// short-circuits instead of re-resolving per sub-call. In isolation it returns
// (and, on first use, opens) brw's owned tab; in follow-focus mode it runs the
// same bounded retry as contextTabID so a mid-reconnect MV3 service worker does
// not drop the call onto a stale tab.
func (b *Bridge) ResolveActiveTabID(ctx context.Context) string {
	return b.ensureOwnedTabID(ctx)
}

var _ browser.ActiveTabReporter = (*Bridge)(nil)

// ActiveTabID satisfies browser.ActiveTabReporter: it is the same resolution
// with the failure named instead of returned as an empty string, so a caller
// that has to report a tab can say why it has none.
func (b *Bridge) ActiveTabID(ctx context.Context) (string, error) {
	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return tabID, nil
	}
	return "", errors.New("the extension bridge resolved no active tab to name")
}

// pinActiveTab resolves the active tab once and returns a context with that tab
// pinned via browser.WithCurrentOwnedTabID. If an explicit tab is already in the context it
// is left untouched (the caller asked for a specific tab). If resolution fails
// (extension disconnected, mid-reconnect) the original context is returned so
// downstream calls keep their existing per-call resolution behaviour rather than
// pinning an empty tab.
func (b *Bridge) pinActiveTab(ctx context.Context) context.Context {
	if browser.TabIDFromContext(ctx) != "" {
		return ctx
	}
	// ensureOwnedTabID (not contextTabID) so a batch/plan that starts before any
	// brw_open also opens its own tab in isolation instead of resolving the user's.
	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return browser.WithCurrentOwnedTabID(ctx, tabID)
	}
	return ctx
}

// retargetPinnedTab re-pins the sequence onto the tab a step just moved focus to
// (focus_tab, open). Without it, a focus_tab/open step mid-batch/plan would leave
// subsequent steps pinned to the STALE pre-focus tab. targetTabID is the KNOWN id
// from the just-completed step (focus_tab's target / open's new tab) — never the
// mutable b.active cache, which async active_tab pushes or concurrent operations
// can change out from under the sequence. base is the caller's context: it
// carries a tab ONLY when the caller passed an explicit tab_id (the MCP/HTTP
// entry excludes batch/plan from one-shot active-tab pinning). stepCtx is the
// currently-pinned context, returned unchanged for non-retargeting steps.
func (b *Bridge) retargetPinnedTab(base, stepCtx context.Context, targetTabID string) context.Context {
	// Non-retargeting step (not focus_tab/open, or it failed): keep the pin.
	if targetTabID == "" {
		return stepCtx
	}
	// An explicitly-supplied tab_id stays sticky for the whole sequence (matching
	// the pre-pin behaviour where contextTabID short-circuits on the caller's tab
	// regardless of focus_tab side effects): never let a focus_tab/open override it.
	if browser.TabIDIsExplicit(base) {
		return stepCtx
	}
	if browser.TabIDRequiresCurrentOwnership(stepCtx) {
		return browser.WithCurrentOwnedTabID(base, targetTabID)
	}
	return browser.WithImplicitTabID(base, targetTabID)
}

// ensureForegroundTab makes the just-opened tab id the genuine foreground tab —
// the one resolveActiveTabID/get_active_tab_id returns — so that every
// SUBSEQUENT no-tab_id page tool (brw_find / brw_read / brw_click / snapshot)
// targets the tab brw_open just opened rather than whatever tab Chrome left
// focused.
//
// Why this is needed: open_tab creates the tab active WITHIN its window, but the
// daemon's contextTabID deliberately distrusts the cached b.active and
// re-resolves the live foreground tab every call (correct, because the user can
// switch tabs manually in a shared Chrome). Two real-Chrome cases leave a
// DIFFERENT tab foreground right after an open, so b.active and the live
// resolver diverge — exactly the reported "brw_open then brw_find hit an
// unrelated Google Chat tab" bug:
//  1. The new tab lands in a window that is not the OS-focused one, so
//     resolveForegroundTabId returns the focused window's active tab.
//  2. Grouping the new tab into a COLLAPSED group deactivates it (a collapsed
//     group cannot hold the active tab), so Chrome activates an adjacent tab.
//
// We detect the divergence and heal it with an explicit focus_tab (which focuses
// the window, activates the tab, and expands its group), then re-verify. A tab
// that still cannot be made foreground is a HARD error rather than a silent
// fall-through onto a stale tab.
func (b *Bridge) ensureForegroundTab(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("open returned no tab id; refusing to fall back to a stale active tab")
	}
	if live := b.resolveActiveTabID(ctx); live == id {
		b.setActiveTabID(id)
		return nil
	}
	// Diverged: the opened tab is not the live foreground tab. Focus it
	// explicitly (window focus + activate + group expand) and re-verify.
	if err := b.FocusTab(ctx, id); err != nil {
		return fmt.Errorf("open: could not focus the opened tab %s: %w", id, err)
	}
	if live := b.resolveActiveTabID(ctx); live != id {
		return fmt.Errorf("open: opened tab %s did not become the active tab (resolver reported %q); the open may have been blocked or the tab was immediately replaced", id, live)
	}
	b.setActiveTabID(id)
	return nil
}

// resolveActiveTabID asks the extension for the truly active/focused tab and
// updates the cached reference to match, healing drift. Returns "" when the
// bridge is disconnected or the query fails so the caller can fall back to the
// last-known cached value.
func (b *Bridge) resolveActiveTabID(ctx context.Context) string {
	id, _ := b.resolveActiveTabIDWithReason(ctx)
	return id
}

// resolveActiveTabIDWithReason also returns the extension's explanation when it
// resolved nothing. An empty reason means the failure was transient (worker
// asleep, mid-reconnect) and the caller may retry or fall back; a populated one
// means the extension positively determined there is no drivable tab, which no
// amount of retrying will change.
func (b *Bridge) resolveActiveTabIDWithReason(ctx context.Context) (string, string) {
	b.mu.RLock()
	connected := b.conn != nil
	b.mu.RUnlock()
	if !connected {
		return "", ""
	}
	raw, err := b.call(ctx, "get_active_tab_id", nil)
	if err != nil {
		return "", ""
	}
	var resp struct {
		TabID int    `json:"tabId"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.TabID == 0 {
		return "", resp.Error
	}
	id := strconv.Itoa(resp.TabID)
	// This is the user's live-focused tab. Only heal the cache toward it in
	// follow-focus mode; in isolation the cache is brw's owned tab and must not be
	// repointed at the user's tab. (In isolation this resolver isn't used for
	// targeting, but guard the write so no path can clobber the owned id.)
	if id != b.activeTabID() && b.followFocus {
		b.setActiveTabID(id)
	}
	return id, ""
}

// isNoDrivableTabReason reports whether the extension positively determined that
// every candidate tab is one Chrome will not let brw drive (a foreign
// extension's page — Bitwarden's popout is the common one — or a chrome://
// surface). Distinct from a transient resolution failure: retrying cannot help,
// and falling back to the cached tab is actively wrong because that cached id is
// very likely the same undrivable tab.
func isNoDrivableTabReason(reason string) bool {
	return strings.Contains(strings.ToLower(reason), "no drivable tab")
}

func isBridgeTabLostError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no tab")
}

func isBridgeDebuggerDetachedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "detached while handling command") ||
		strings.Contains(msg, "debugger is not attached") ||
		strings.Contains(msg, "target closed")
}
