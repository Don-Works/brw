package extensionbridge

import (
	"context"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// SetWebMCP enables the opt-in WebMCP runtime (--enable-webmcp) on this transport.
func (b *Bridge) SetWebMCP(enabled bool) { b.webmcp = enabled }

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

func (b *Bridge) cachedTabID(ctx context.Context) string {
	if !b.webmcp {
		return ""
	}
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	return b.activeTabID()
}
