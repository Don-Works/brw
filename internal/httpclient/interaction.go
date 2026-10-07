package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

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

func (c *Controller) Focus(ctx context.Context, ref string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/focus", map[string]any{"ref": ref, "snapshot": browser.WantSnapshotFromCtx(ctx)}, &out)
	return out, err
}

func (c *Controller) FocusRef(ctx context.Context, ref string) error {
	_, err := c.Focus(ctx, ref)
	return err
}
