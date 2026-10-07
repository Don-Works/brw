package extensionbridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
)

type tabArm struct {
	mu    sync.Mutex
	armed map[string]bool
}

func (c *tabArm) claim(tabID string) bool {
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

func (c *tabArm) release(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.armed, tabID)
}

func (c *tabArm) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = nil
}

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
		b.containment.release(tabID)
	}
}

// BlockedRequests reports (and clears) the subresources containment refused on a tab.
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
