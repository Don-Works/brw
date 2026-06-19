package browser

import (
	"context"
	"errors"
)

// ErrTabGroupingUnsupported is returned by the direct-CDP transport for any
// tab-grouping operation. Chrome tab groups are a browser-UI feature owned by
// the chrome:// layer and exposed only through the chrome.tabGroups /
// chrome.tabs.group extension APIs. The DevTools Protocol (the Target, Browser,
// and Page domains this Manager drives) has no primitive to create a tab group
// or assign a target to one — cdproto exposes none, and Chromium ships none.
//
// Rather than silently returning nil (reporting success while no group is ever
// created — the dead-wiring bug this replaces), the direct-CDP Manager fails
// loudly so callers can fall back to the extension-bridge transport, which does
// implement grouping via the extension APIs.
var ErrTabGroupingUnsupported = errors.New("tab grouping is not supported on the direct-CDP transport (Chrome tab groups are exposed only via the extension APIs, not the DevTools Protocol); use the extension-bridge transport for grouping")

// OpenInGroup opens the URL but cannot place the resulting tab into a named
// Chrome tab group over direct CDP. Because the "in group" guarantee cannot be
// honored, it reports ErrTabGroupingUnsupported instead of fabricating success.
func (m *Manager) OpenInGroup(ctx context.Context, url string, opts TabGroupOptions) (OpenResult, error) {
	return OpenResult{}, ErrTabGroupingUnsupported
}

// ListTabGroups cannot inspect Chrome tab groups over direct CDP.
func (m *Manager) ListTabGroups(ctx context.Context) ([]TabGroup, error) {
	return nil, ErrTabGroupingUnsupported
}

// GroupTabs cannot create or assign Chrome tab groups over direct CDP.
func (m *Manager) GroupTabs(ctx context.Context, tabIDs []string, opts TabGroupOptions) error {
	return ErrTabGroupingUnsupported
}

// UngroupTabs cannot remove tabs from a Chrome tab group over direct CDP.
func (m *Manager) UngroupTabs(ctx context.Context, tabIDs []string) error {
	return ErrTabGroupingUnsupported
}
