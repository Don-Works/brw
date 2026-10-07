package browser

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed scripts/screenshot_prepare.js
var screenshotPrepareScript string

//go:embed scripts/screenshot_encode.js
var screenshotEncodeScript string

var screenshotSaveGate = make(chan struct{}, 1)

// LockScreenshotSave serializes temporary capture state and respects cancellation.
func LockScreenshotSave(ctx context.Context) (func(), error) {
	select {
	case screenshotSaveGate <- struct{}{}:
		return func() { <-screenshotSaveGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ScreenshotExpressions builds shared preparation and restoration for both transports.
func ScreenshotExpressions(opts ScreenshotSaveOptions) (string, string) {
	data, _ := json.Marshal(map[string]any{
		"scale": opts.Scale, "full_page": opts.FullPage, "ref": opts.Ref,
		"region": opts.Region, "hide": opts.Hide, "settle_ms": opts.SettleMS,
	})
	key, _ := json.Marshal("__brwCapture_" + rand.Text())
	prepare := screenshotPrepareScript + `(` + string(data) + `,` + string(key) + `,` + snapshot.ResolveOrRecoverBoxScript + `)`
	cleanup := `window[` + string(key) + `]?.()`
	return prepare, cleanup
}

// ScreenshotEncoder returns the bitmap crop encoder for an isolated browser world.
func ScreenshotEncoder() string { return screenshotEncodeScript }

// SaveScreenshot captures compositor pixels without altering the viewport's device emulation.
func (m *Manager) SaveScreenshot(ctx context.Context, opts ScreenshotSaveOptions) (SavedScreenshot, error) {
	if err := m.refuseOnRemote("local_screenshot_files"); err != nil {
		return SavedScreenshot{}, err
	}
	if err := GuardCrossOriginRefs("screenshot save", DirectCrossOriginRemedy, opts.Ref); err != nil {
		return SavedScreenshot{}, err
	}
	unlock, err := LockScreenshotSave(ctx)
	if err != nil {
		return SavedScreenshot{}, err
	}
	defer unlock()
	return SaveScreenshotFile(ctx, opts, func(o ScreenshotSaveOptions) (shot Screenshot, err error) {
		tabID, tabCtx, cancel, err := m.activeContext(ctx)
		if err != nil {
			return shot, err
		}
		defer cancel()
		prepare, cleanup := ScreenshotExpressions(o)
		err = chromedp.Run(tabCtx, chromedp.ActionFunc(func(execCtx context.Context) (err error) {
			defer func() {
				cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(execCtx), 3*time.Second)
				defer cancel()
				_, exception, cleanErr := runtime.Evaluate(cleanup).Do(cleanCtx)
				if exception != nil {
					cleanErr = exception
				}
				if o.OmitBackground {
					cleanErr = errors.Join(cleanErr, emulation.SetDefaultBackgroundColorOverride().Do(cleanCtx))
				}
				err = errors.Join(err, cleanErr)
			}()
			result, exception, err := runtime.Evaluate(prepare).WithAwaitPromise(true).WithReturnByValue(true).Do(execCtx)
			if err != nil {
				return err
			}
			if exception != nil {
				return exception
			}
			var plan struct {
				Clip   *page.Viewport    `json:"clip"`
				Crop   *ScreenshotRegion `json:"crop"`
				Width  int               `json:"width"`
				Height int               `json:"height"`
			}
			if err := json.Unmarshal(result.Value, &plan); err != nil {
				return err
			}
			if plan.Clip == nil {
				return errors.New("capture preparation returned no clip")
			}
			if o.OmitBackground {
				if err := emulation.SetDefaultBackgroundColorOverride().WithColor(&cdp.RGBA{R: 0, G: 0, B: 0, A: 0}).Do(execCtx); err != nil {
					return err
				}
			}
			params := map[string]any{"format": o.Format, "fromSurface": true, "captureBeyondViewport": true, "clip": plan.Clip}
			if plan.Crop != nil {
				params["format"] = "png"
			}
			if o.Format != "png" && plan.Crop == nil {
				quality := 90
				if o.Quality != nil {
					quality = *o.Quality
				}
				params["quality"] = quality
			}
			var response struct {
				Data string `json:"data"`
			}
			if err := cdp.Execute(execCtx, "Page.captureScreenshot", params, &response); err != nil {
				return err
			}
			if plan.Crop != nil {
				tree, err := page.GetFrameTree().Do(execCtx)
				if err != nil {
					return err
				}
				world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("brw screenshot encoding").Do(execCtx)
				if err != nil {
					return err
				}
				quality := 90
				if o.Quality != nil {
					quality = *o.Quality
				}
				args, _ := json.Marshal([]any{response.Data, plan.Crop, o.Format, quality})
				encoded, exception, err := runtime.Evaluate(screenshotEncodeScript + `.apply(null,` + string(args) + `)`).WithContextID(world).WithAwaitPromise(true).WithReturnByValue(true).Do(execCtx)
				if err != nil {
					return err
				}
				if exception != nil {
					return exception
				}
				if err := json.Unmarshal(encoded.Value, &response.Data); err != nil {
					return err
				}
			}
			shot.Data, err = base64.StdEncoding.DecodeString(response.Data)
			if err == nil {
				err = CheckScreenshotDimensions(shot.Data, plan.Width, plan.Height)
			}

			shot.MIMEType = "image/" + o.Format
			return err
		}))
		if err == nil {
			err = m.guardCurrentURL(tabID, tabCtx)
		}
		if err != nil {
			return Screenshot{}, err
		}
		return shot, err
	})
}
