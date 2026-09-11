package extensionbridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
)

// containmentArm tracks which tabs have already had containment installed, so
// arming is one message per tab rather than one per navigation.
type containmentArm struct {
	mu    sync.Mutex
	armed map[string]bool
}

func (c *containmentArm) claim(tabID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.armed == nil {
		c.armed = make(map[string]bool)
	}
	if c.armed[tabID] {
		return false
	}
	c.armed[tabID] = true
	return true
}

func (c *containmentArm) release(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.armed, tabID)
}

// ensureContainment installs subresource containment on a tab before brw drives
// it.
//
// It must run BEFORE the navigation it protects: the in-page guard is installed
// with Page.addScriptToEvaluateOnNewDocument, which only beats page scripts for
// documents that have not loaded yet.
//
// On the extension lane brw attaches to a tab in the user's real, already-running
// Chrome, so the guarantee is narrower than on a browser brw launched itself:
// requests issued before brw attached were never seen, and the catch-up
// injection into an already-loaded document can lose a race with page code that
// already captured the originals. HTTP requests stay covered by Fetch
// interception either way.
func (b *Bridge) ensureContainment(ctx context.Context, tabID string) {
	if !b.navPolicy.Confines() {
		return
	}
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return
	}
	if !b.containment.claim(tabID) {
		return
	}
	guard, err := browser.BuildContainmentGuard(b.navPolicy)
	if err != nil {
		b.containment.release(tabID)
		return
	}
	params := map[string]any{
		"tabId":   parseTabID(tabID),
		"enabled": true,
		"allowed": b.navPolicy.Allowed,
		"blocked": b.navPolicy.Blocked,
		"guard":   guard,
	}
	if _, err := b.call(ctx, "set_containment", params); err != nil {
		// A failed arm must not be remembered as armed, or the tab would run
		// uncontained while the daemon believed otherwise.
		b.containment.release(tabID)
	}
}

// BlockedRequests reports (and clears) the subresources containment refused on a
// tab. A contained page that half-renders is otherwise an unexplained bug.
func (b *Bridge) BlockedRequests(ctx context.Context, tabID string) ([]browser.BlockedRequest, error) {
	params := map[string]any{}
	if strings.TrimSpace(tabID) != "" {
		params["tabId"] = parseTabID(tabID)
	}
	raw, err := b.call(ctx, "get_blocked_requests", params)
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return []browser.BlockedRequest{}, nil
		}
		return nil, err
	}
	var payload struct {
		Blocked []browser.BlockedRequest `json:"blocked"`
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return nil, jsonErr
		}
	}
	if payload.Blocked == nil {
		payload.Blocked = []browser.BlockedRequest{}
	}
	return payload.Blocked, nil
}
