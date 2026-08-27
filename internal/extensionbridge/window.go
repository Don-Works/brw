package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Don-Works/brw/internal/browser"
)

// windowResizeUnsupportedNote explains a bridge that predates resize support,
// so an agent can tell "your extension is old" apart from "the resize failed".
const windowResizeUnsupportedNote = "this brw extension build predates window resizing; update the extension to resize the browser window"

// ResizeWindow moves and resizes the real OS window hosting a tab, via the
// extension's chrome.windows API. The direct-CDP backend does the same thing
// through Browser.setWindowBounds; both validate identically so an agent gets
// the same errors whichever transport it is on.
func (b *Bridge) ResizeWindow(ctx context.Context, opts browser.WindowResizeOptions) (browser.WindowResizeResult, error) {
	if err := browser.ValidateWindowResize(opts); err != nil {
		return browser.WindowResizeResult{}, err
	}
	state, err := browser.NormalizeWindowState(opts.State)
	if err != nil {
		return browser.WindowResizeResult{}, err
	}

	// Resolve through the same path every page tool uses, so an explicit
	// tab_id, an HTTP session lease, and the session's working tab all behave
	// here exactly as they do elsewhere.
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return browser.WindowResizeResult{}, errors.New("no tab to resize: open or focus a tab first, or pass tab_id")
	}

	params := map[string]any{"tabId": parseTabID(tabID)}
	if opts.Width > 0 {
		params["width"] = opts.Width
	}
	if opts.Height > 0 {
		params["height"] = opts.Height
	}
	if opts.Left != nil {
		params["left"] = *opts.Left
	}
	if opts.Top != nil {
		params["top"] = *opts.Top
	}
	if state != "" {
		params["state"] = state
	}

	raw, err := b.call(ctx, "resize_window", params)
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return browser.WindowResizeResult{Note: windowResizeUnsupportedNote}, nil
		}
		return browser.WindowResizeResult{}, err
	}

	var result browser.WindowResizeResult
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &result); jsonErr != nil {
			return browser.WindowResizeResult{}, fmt.Errorf("parse resize result: %w", jsonErr)
		}
	}
	result.OK = true
	if opts.Width > 0 && result.Width != opts.Width {
		result.Clamped = true
	}
	if opts.Height > 0 && result.Height != opts.Height {
		result.Clamped = true
	}
	if state != "" && state != "normal" {
		// A maximize is expected to land on a different size; that is not a clamp.
		result.Clamped = false
	}
	if result.Clamped {
		result.Note = "Chrome clamped the window to the available display area"
	}
	return result, nil
}
