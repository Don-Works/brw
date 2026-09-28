package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Don-Works/brw/internal/browser"
)

// SaveScreenshot captures the active compositor surface and saves on the bridge host.
func (b *Bridge) SaveScreenshot(ctx context.Context, opts browser.ScreenshotSaveOptions) (browser.SavedScreenshot, error) {
	if err := browser.GuardCrossOriginRefs("screenshot save", browser.BridgeCrossOriginRemedy, opts.Ref); err != nil {
		return browser.SavedScreenshot{}, err
	}
	unlock, err := browser.LockScreenshotSave(ctx)
	if err != nil {
		return browser.SavedScreenshot{}, err
	}
	defer unlock()
	return browser.SaveScreenshotFile(ctx, opts, func(o browser.ScreenshotSaveOptions) (browser.Screenshot, error) {
		prepare, cleanup := browser.ScreenshotExpressions(o)
		quality := 90
		if o.Quality != nil {
			quality = *o.Quality
		}
		raw, err := b.call(ctx, "capture_presentation", map[string]any{
			"tabId":  parseTabID(b.contextTabID(ctx)),
			"params": map[string]any{"prepare": prepare, "encode": browser.ScreenshotEncoder(), "cleanup": cleanup, "format": o.Format, "quality": quality, "omitBackground": o.OmitBackground},
		})
		if err != nil {
			if isUnknownMessageTypeErr(err) {
				return browser.Screenshot{}, fmt.Errorf("reload the brw extension to enable presentation screenshots: %w", err)
			}
			return browser.Screenshot{}, err
		}
		shot, err := screenshotFromRawMIME(raw, "image/"+o.Format)
		shot.Base64 = ""
		if err == nil {
			var dimensions struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			}
			if err = json.Unmarshal(raw, &dimensions); err == nil {
				err = browser.CheckScreenshotDimensions(shot.Data, dimensions.Width, dimensions.Height)
			}
		}
		return shot, err
	})
}
