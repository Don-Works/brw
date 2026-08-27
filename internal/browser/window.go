package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/target"
)

// WindowResizeOptions describes a real OS browser-window change. This is not
// device emulation: EmulateDevice overrides viewport metrics inside the
// renderer, whereas this moves and resizes the window a human actually sees,
// which is what desktop-aware layouts and window-manager-sensitive apps key off.
type WindowResizeOptions struct {
	Width  int  `json:"width,omitempty"`
	Height int  `json:"height,omitempty"`
	Left   *int `json:"left,omitempty"`
	Top    *int `json:"top,omitempty"`
	// State is normal, minimized, maximized, or fullscreen. Chrome rejects
	// bounds supplied alongside a non-normal state, so the two are applied in
	// separate calls.
	State string `json:"state,omitempty"`
}

// WindowResizeResult reports the geometry Chrome settled on, so a caller can
// confirm the change without a second round trip. Chrome clamps to the display,
// so a requested size and the applied size legitimately differ.
type WindowResizeResult struct {
	OK       bool   `json:"ok"`
	WindowID int    `json:"window_id,omitempty"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Left     int    `json:"left"`
	Top      int    `json:"top"`
	State    string `json:"state,omitempty"`
	// Clamped reports that Chrome applied a different size from the one asked
	// for, normally because the request exceeded the display.
	Clamped bool   `json:"clamped,omitempty"`
	Note    string `json:"note,omitempty"`
}

// validWindowStates are the states Chrome's WindowState accepts.
var validWindowStates = map[string]browser.WindowState{
	"normal":     browser.WindowStateNormal,
	"minimized":  browser.WindowStateMinimized,
	"maximized":  browser.WindowStateMaximized,
	"fullscreen": browser.WindowStateFullscreen,
}

// NormalizeWindowState validates a requested window state, returning the empty
// string when none was asked for. Shared by both transports so the extension
// bridge and direct CDP reject exactly the same inputs.
func NormalizeWindowState(state string) (string, error) {
	clean := strings.ToLower(strings.TrimSpace(state))
	if clean == "" {
		return "", nil
	}
	if _, ok := validWindowStates[clean]; !ok {
		return "", fmt.Errorf("unknown window state %q (valid: normal, minimized, maximized, fullscreen)", state)
	}
	return clean, nil
}

// ValidateWindowResize rejects a request that would do nothing, so a caller
// gets an error rather than a success that changed nothing.
func ValidateWindowResize(opts WindowResizeOptions) error {
	if opts.Width < 0 || opts.Height < 0 {
		return errors.New("width and height must not be negative")
	}
	if opts.Width == 0 && opts.Height == 0 && opts.Left == nil && opts.Top == nil && opts.State == "" {
		return errors.New("nothing to change: pass width/height, left/top, or state")
	}
	return nil
}

// ResizeWindow moves and resizes the real OS window hosting the active tab.
func (m *Manager) ResizeWindow(ctx context.Context, opts WindowResizeOptions) (WindowResizeResult, error) {
	if err := ValidateWindowResize(opts); err != nil {
		return WindowResizeResult{}, err
	}
	state, err := NormalizeWindowState(opts.State)
	if err != nil {
		return WindowResizeResult{}, err
	}

	tabID, err := m.ensureActive(ctx)
	if err != nil {
		return WindowResizeResult{}, err
	}

	var result WindowResizeResult
	runErr := m.runBrowser(ctx, func(ctx context.Context) error {
		windowID, current, err := browser.GetWindowForTarget().
			WithTargetID(target.ID(tabID)).Do(ctx)
		if err != nil {
			return fmt.Errorf("resolve window for tab: %w", err)
		}

		// Chrome refuses bounds sent together with a non-normal state, and a
		// window that is currently minimized/maximized ignores bounds entirely.
		// Restore to normal first when geometry was requested, then apply it.
		wantsGeometry := opts.Width > 0 || opts.Height > 0 || opts.Left != nil || opts.Top != nil
		priorState := browser.WindowStateNormal
		if current != nil {
			priorState = current.WindowState
		}
		if wantsGeometry && priorState != browser.WindowStateNormal {
			if err := browser.SetWindowBounds(windowID, &browser.Bounds{
				WindowState: browser.WindowStateNormal,
			}).Do(ctx); err != nil {
				return fmt.Errorf("restore window to normal: %w", err)
			}
			// Asking for a width is not asking to be shown. Unless the caller
			// named a state, the window goes back to the one it was in, so a
			// size-only request cannot unminimize and expose a window.
			if state == "" {
				defer func() {
					_ = browser.SetWindowBounds(windowID, &browser.Bounds{
						WindowState: priorState,
					}).Do(ctx)
				}()
			}
		}

		if wantsGeometry {
			bounds := &browser.Bounds{}
			if opts.Width > 0 {
				bounds.Width = int64(opts.Width)
			}
			if opts.Height > 0 {
				bounds.Height = int64(opts.Height)
			}
			if opts.Left != nil {
				bounds.Left = int64(*opts.Left)
			}
			if opts.Top != nil {
				bounds.Top = int64(*opts.Top)
			}
			if err := browser.SetWindowBounds(windowID, bounds).Do(ctx); err != nil {
				return fmt.Errorf("set window bounds: %w", err)
			}
		}

		// The state change goes last and alone, so a caller can size a window
		// and then maximize it in one request without Chrome rejecting the pair.
		if state != "" && state != "normal" {
			if err := browser.SetWindowBounds(windowID, &browser.Bounds{
				WindowState: validWindowStates[state],
			}).Do(ctx); err != nil {
				return fmt.Errorf("set window state: %w", err)
			}
		} else if state == "normal" && !wantsGeometry {
			if err := browser.SetWindowBounds(windowID, &browser.Bounds{
				WindowState: browser.WindowStateNormal,
			}).Do(ctx); err != nil {
				return fmt.Errorf("set window state: %w", err)
			}
		}

		// Asking for a width is not asking to be shown. When the caller named no
		// state, put the window back the way it was, so a size-only request
		// cannot unminimize and expose a window. This runs BEFORE the read-back,
		// so the reported state is the one the window actually ends in.
		if wantsGeometry && state == "" && priorState != browser.WindowStateNormal {
			if err := browser.SetWindowBounds(windowID, &browser.Bounds{
				WindowState: priorState,
			}).Do(ctx); err != nil {
				return fmt.Errorf("restore prior window state: %w", err)
			}
		}

		// Read back what Chrome settled on rather than echoing the request:
		// Chrome clamps to the display, so the two genuinely differ.
		_, applied, err := browser.GetWindowForTarget().
			WithTargetID(target.ID(tabID)).Do(ctx)
		if err != nil {
			return fmt.Errorf("read back window bounds: %w", err)
		}
		result = windowResultFrom(int(windowID), applied)
		return nil
	})
	if runErr != nil {
		return WindowResizeResult{}, runErr
	}

	result.OK = true
	result.Clamped = resizeWasClamped(opts, result)
	if result.Clamped {
		result.Note = "Chrome clamped the window to the available display area"
	}
	return result, nil
}

func windowResultFrom(windowID int, bounds *browser.Bounds) WindowResizeResult {
	out := WindowResizeResult{WindowID: windowID}
	if bounds == nil {
		return out
	}
	out.Width = int(bounds.Width)
	out.Height = int(bounds.Height)
	out.Left = int(bounds.Left)
	out.Top = int(bounds.Top)
	out.State = string(bounds.WindowState)
	return out
}

// resizeWasClamped reports whether Chrome applied a different size from the one
// requested. Only meaningful for a plain geometry change: a maximize is
// expected to land on a different size and is not a clamp.
func resizeWasClamped(opts WindowResizeOptions, applied WindowResizeResult) bool {
	// Compare the NORMALIZED state: " Normal " and "normal" mean the same thing
	// to the validator, so they must mean the same thing here, or the two
	// backends disagree about whether a request was clamped.
	if state, err := NormalizeWindowState(opts.State); err == nil && state != "" && state != "normal" {
		return false
	}
	if opts.Width > 0 && applied.Width != opts.Width {
		return true
	}
	if opts.Height > 0 && applied.Height != opts.Height {
		return true
	}
	return false
}
