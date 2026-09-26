package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

// SaveScreenshot forwards the destination and returns only metadata and a preview.
func (c *Controller) SaveScreenshot(ctx context.Context, opts browser.ScreenshotSaveOptions) (browser.SavedScreenshot, error) {
	var result browser.SavedScreenshot
	err := c.post(ctx, "/api/visual/screenshot_save", opts, &result)
	return result, err
}
