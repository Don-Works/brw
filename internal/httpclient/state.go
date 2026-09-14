package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

// maxSessionStateResponseBytes bounds the answer. A session-state response is
// ids, origins and counts; anything approaching this size means the daemon on
// the other end is not answering the shape this route promises.
const maxSessionStateResponseBytes = int64(256 << 10)

// SessionState forwards brw_state to the browser host. Only the action, the
// origin allowlist and an opaque snapshot id travel out; only metadata and
// counts come back. The snapshot is written, read and decrypted on the host, so
// an --upstream-http session never puts session material on the wire.
func (c *Controller) SessionState(ctx context.Context, opts browser.SessionStateOptions) (browser.SessionStateResult, error) {
	var out browser.SessionStateResult
	// postExactWithLimit, not post: a session-state call is addressed to a
	// browser CONTEXT, not a tab, so the tab_id/snapshot fields the ordinary
	// page path folds in are both meaningless here and rejected by the strict
	// decoder on the other side. It also keeps the response ceiling at the size
	// of a metadata document.
	err := c.postExactWithLimit(ctx, "/api/browser/state", opts, &out, maxSessionStateResponseBytes)
	return out, err
}

var _ browser.SessionStateController = (*Controller)(nil)
