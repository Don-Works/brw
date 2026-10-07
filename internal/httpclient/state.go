package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

const maxSessionStateResponseBytes = int64(256 << 10)

// SessionState forwards brw_state to the browser host.
func (c *Controller) SessionState(ctx context.Context, opts browser.SessionStateOptions) (browser.SessionStateResult, error) {
	var out browser.SessionStateResult

	err := c.postExactWithLimit(ctx, "/api/browser/state", opts, &out, maxSessionStateResponseBytes)
	return out, err
}

var _ browser.SessionStateController = (*Controller)(nil)
