package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

// The proxy transport forwards the interaction long tail to the daemon that
// owns the browser. Each of these is an optional browser.Controller capability
// (ClipboardController, KeyHoldController, HistoryController, ElementFocuser):
// the upstream daemon answers with its own transport's capability error when it
// cannot do the work, so the refusal an agent sees names the real reason rather
// than "the proxy does not implement it".

func (c *Controller) Clipboard(ctx context.Context, opts browser.ClipboardOptions) (browser.ClipboardResult, error) {
	var out browser.ClipboardResult
	err := c.post(ctx, "/api/page/clipboard", opts, &out)
	return out, err
}

func (c *Controller) KeyDown(ctx context.Context, opts browser.KeyHoldOptions) (browser.KeyHoldResult, error) {
	var out browser.KeyHoldResult
	err := c.post(ctx, "/api/page/key_down", opts, &out)
	return out, err
}

func (c *Controller) KeyUp(ctx context.Context, opts browser.KeyHoldOptions) (browser.KeyHoldResult, error) {
	var out browser.KeyHoldResult
	err := c.post(ctx, "/api/page/key_up", opts, &out)
	return out, err
}

func (c *Controller) PushState(ctx context.Context, opts browser.HistoryStateOptions) (browser.HistoryStateResult, error) {
	var out browser.HistoryStateResult
	err := c.post(ctx, "/api/page/pushstate", opts, &out)
	return out, err
}

func (c *Controller) FocusRef(ctx context.Context, ref string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	return c.post(ctx, "/api/page/focus", map[string]string{"ref": ref}, &out)
}
