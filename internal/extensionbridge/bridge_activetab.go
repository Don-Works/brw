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

// contextTabID resolves the tab a page action targets. In follow-focus mode
// with no tab_id it costs one get_active_tab_id RPC per call, because the cached
// b.active drifts when the user switches tabs; pass tab_id or use brw_batch /
// brw_plan to avoid it.
func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if !b.followFocus {
		// Isolation: b.active is written only by brw's own Open/FocusTab. "" means no
		// owned tab yet; ensureOwnedTabID opens one.
		return b.activeTabID()
	}
	// Retry before trusting the cache: the MV3 worker may be mid-reconnect, and
	// one transient failure falling to a stale tab is how read, observe and
	// snapshot ended up on three different tabs.
	for attempt := 0; attempt < activeTabResolveAttempts; attempt++ {
		live, reason := b.resolveActiveTabIDWithReason(ctx)
		if live != "" {
			return live
		}
		// "No drivable tab" is definitive, and the cached tab is usually the
		// undrivable one. Return "" so the extension fails with the actionable reason.
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

// ensureOwnedTabID resolves the tab a top-level no-tab_id call targets. An
// explicit tab_id wins. In isolation it returns brw's owned tab, opening a
// background one in the default group if none; in follow-focus it defers to
// contextTabID.
func (b *Bridge) ensureOwnedTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if b.followFocus {
		return b.contextTabID(ctx)
	}
	owned, reconciled := b.reconciledOwnedTab()
	if !reconciled {
		// Do not trust ownership cached from a displaced worker. getConn parks through
		// the MV3 gap and handleExtension reconciles agent_tab_id before waking it.
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
	// A cooldown after failure plus a bounded timeout stop a wedged browser from
	// turning each no-tab_id call into a full-timeout hang.
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
	b.mu.Lock()
	b.autoOpenFailedAt = time.Time{}
	b.mu.Unlock()
	return res.Tab.ID
}

// reconciledOwnedTab returns the isolation pin only once the current connection's
// hello has confirmed it; a same-worker reconnect may retain a stale b.active.
func (b *Bridge) reconciledOwnedTab() (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active, b.conn != nil && b.agentPinKnown && !b.shuttingDown
}

// ResolveActiveTabID resolves (in isolation, opening if needed) the tab a
// no-tab_id tool call targets, or "". Entry points pin the result with
// browser.WithCurrentOwnedTabID so sub-calls skip re-resolution.
func (b *Bridge) ResolveActiveTabID(ctx context.Context) string {
	return b.ensureOwnedTabID(ctx)
}

var _ browser.ActiveTabReporter = (*Bridge)(nil)

// ActiveTabID is ResolveActiveTabID with the failure named instead of "".
func (b *Bridge) ActiveTabID(ctx context.Context) (string, error) {
	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return tabID, nil
	}
	return "", errors.New("the extension bridge resolved no active tab to name")
}

// pinActiveTab pins the resolved tab into ctx once. An explicit tab is left
// alone; on failure ctx is returned unchanged rather than pinning "".
func (b *Bridge) pinActiveTab(ctx context.Context) context.Context {
	if browser.TabIDFromContext(ctx) != "" {
		return ctx
	}
	// ensureOwnedTabID so a batch/plan before any brw_open gets its own tab in isolation.
	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return browser.WithCurrentOwnedTabID(ctx, tabID)
	}
	return ctx
}

// retargetPinnedTab re-pins a sequence onto the tab a focus_tab/open step just
// moved to, so later steps do not hit the pre-focus tab. targetTabID comes from
// the step itself, never b.active, which async pushes can change. base carries a
// tab only when the caller gave an explicit tab_id, which stays sticky.
func (b *Bridge) retargetPinnedTab(base, stepCtx context.Context, targetTabID string) context.Context {
	if targetTabID == "" {
		return stepCtx
	}
	if browser.TabIDIsExplicit(base) {
		return stepCtx
	}
	if browser.TabIDRequiresCurrentOwnership(stepCtx) {
		return browser.WithCurrentOwnedTabID(base, targetTabID)
	}
	return browser.WithImplicitTabID(base, targetTabID)
}

// ensureForegroundTab makes the just-opened tab the live foreground tab so the
// next no-tab_id tool targets it. Chrome can leave a different tab foreground
// after open_tab: the tab lands in a window that is not OS-focused, or joining
// a COLLAPSED group deactivates it. Diverged? focus_tab and re-verify; still
// diverged is a hard error, not a fall-through to a stale tab.
func (b *Bridge) ensureForegroundTab(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("open returned no tab id; refusing to fall back to a stale active tab")
	}
	if live := b.resolveActiveTabID(ctx); live == id {
		b.setActiveTabID(id)
		return nil
	}
	if err := b.FocusTab(ctx, id); err != nil {
		return fmt.Errorf("open: could not focus the opened tab %s: %w", id, err)
	}
	if live := b.resolveActiveTabID(ctx); live != id {
		return fmt.Errorf("open: opened tab %s did not become the active tab (resolver reported %q); the open may have been blocked or the tab was immediately replaced", id, live)
	}
	b.setActiveTabID(id)
	return nil
}

// resolveActiveTabID asks the extension for the focused tab. "" means
// disconnected or failed.
func (b *Bridge) resolveActiveTabID(ctx context.Context) string {
	id, _ := b.resolveActiveTabIDWithReason(ctx)
	return id
}

// resolveActiveTabIDWithReason also returns the extension's reason when it
// resolved nothing. Empty reason: transient, retry. Non-empty: no drivable tab.
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
	// Only follow-focus may repoint the cache at the user's tab; in isolation it is
	// brw's owned tab.
	if id != b.activeTabID() && b.followFocus {
		b.setActiveTabID(id)
	}
	return id, ""
}

// isNoDrivableTabReason: every candidate is a tab Chrome will not let brw drive
// (another extension's page such as Bitwarden's popout, or chrome://).
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
