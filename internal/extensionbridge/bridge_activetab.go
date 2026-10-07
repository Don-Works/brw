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

func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if !b.followFocus {
		return b.activeTabID()
	}

	for attempt := 0; attempt < activeTabResolveAttempts; attempt++ {
		live, reason := b.resolveActiveTabIDWithReason(ctx)
		if live != "" {
			return live
		}

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

func (b *Bridge) ensureOwnedTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if b.followFocus {
		return b.contextTabID(ctx)
	}
	owned, reconciled := b.reconciledOwnedTab()
	if !reconciled {
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

func (b *Bridge) reconciledOwnedTab() (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active, b.conn != nil && b.agentPinKnown && !b.shuttingDown
}

// ResolveActiveTabID resolves (in isolation, opening if needed) the tab a no-tab_id tool call targets, or "".
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

func (b *Bridge) pinActiveTab(ctx context.Context) context.Context {
	if browser.TabIDFromContext(ctx) != "" {
		return ctx
	}

	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return browser.WithCurrentOwnedTabID(ctx, tabID)
	}
	return ctx
}

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

func (b *Bridge) resolveActiveTabID(ctx context.Context) string {
	id, _ := b.resolveActiveTabIDWithReason(ctx)
	return id
}

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

	if id != b.activeTabID() && b.followFocus {
		b.setActiveTabID(id)
	}
	return id, ""
}

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
