package extensionbridge

import (
	"context"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// SetWebMCP enables the opt-in WebMCP runtime (--enable-webmcp) on this
// transport. Call before serving.
func (b *Bridge) SetWebMCP(enabled bool) { b.webmcp = enabled }

// ensureWebMCP arms the WebMCP shim on a tab: registered at document-start for
// every later navigation, plus a catch-up run in the document already loaded.
// Once per tab per extension connection; the extension re-registers it by itself
// whenever it re-attaches its debugger to the tab.
//
// An extension older than set_webmcp is remembered as armed so it is not asked
// again on every call; page tools there see only a native document.modelContext.
func (b *Bridge) ensureWebMCP(ctx context.Context, tabID string) {
	if !b.webmcp {
		return
	}
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return
	}
	if !b.webmcpArm.claim(tabID) {
		return
	}
	params := map[string]any{
		"tabId":   parseTabID(tabID),
		"enabled": true,
		"source":  snapshot.WebMCPInstallScript,
		"catchUp": true,
	}
	if _, err := b.call(ctx, "set_webmcp", params); err != nil && !isUnknownMessageTypeErr(err) {
		b.webmcpArm.release(tabID)
	}
}

// noteOpenedWebMCP records a tab open_tab already armed before its first
// navigation. An extension that could not arm it (or predates the param) gets
// the ordinary arm, whose catch-up covers the document that is loading.
func (b *Bridge) noteOpenedWebMCP(ctx context.Context, tabID string, armed bool) {
	if !b.webmcp {
		return
	}
	if armed {
		b.webmcpArm.claim(strings.TrimSpace(tabID))
		return
	}
	b.ensureWebMCP(ctx, tabID)
}

// cachedTabID names the tab a call targets without a round trip to the
// extension: the tab pinned in the context, else the one brw last drove. In
// follow-focus mode that can lag the user's tab, which only delays the arm to
// the next call that names the tab.
func (b *Bridge) cachedTabID(ctx context.Context) string {
	if !b.webmcp {
		return ""
	}
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	return b.activeTabID()
}
