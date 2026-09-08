package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// ScreencastOptions configures a compositor frame stream.
type ScreencastOptions struct {
	Quality       int
	MaxWidth      int
	MaxHeight     int
	EveryNthFrame int
}

// ScreencastFrame is one JPEG the compositor produced.
type ScreencastFrame struct {
	Data []byte
}

// ScreencastFrames streams frames from Chrome's compositor for the active tab
// until ctx is cancelled, then stops the screencast and closes the channel.
//
// This replaces per-frame Page.captureScreenshot round trips for video
// capture. Chrome pushes a frame only when the page actually changes, so an
// idle page costs nothing, and each frame must be acked or the stream stalls
// after the first one.
//
// The returned stop function ends the screencast and closes the channel. It
// is idempotent and must be called; teardown deliberately does NOT hang off
// the caller's ctx, because tabCtx derives from it and a cancelled tabCtx
// cannot carry the Page.stopScreencast call that releases Chrome's encoder.
//
// Direct CDP only. The extension bridge has no equivalent, and callers fall
// back to the screenshot loop when a controller does not implement this.
func (m *Manager) ScreencastFrames(ctx context.Context, opts ScreencastOptions) (<-chan ScreencastFrame, func(), error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	quality := opts.Quality
	if quality <= 0 || quality > 100 {
		quality = 75
	}
	every := opts.EveryNthFrame
	if every <= 0 {
		every = 1
	}

	frames := make(chan ScreencastFrame, 8)

	// Register the listener BEFORE starting, so the first frame — the one an
	// otherwise-static page depends on — cannot arrive before we are listening.
	chromedp.ListenTarget(tabCtx, func(ev any) {
		e, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		// Ack first and unconditionally: an unacked frame stops the stream, so
		// a full consumer channel must never also cost us the screencast.
		go func() {
			ackCtx, ackCancel := context.WithTimeout(tabCtx, m.timeout)
			defer ackCancel()
			_ = chromedp.Run(ackCtx, page.ScreencastFrameAck(e.SessionID))
		}()
		data, decodeErr := base64.StdEncoding.DecodeString(e.Data)
		if decodeErr != nil || len(data) == 0 {
			return
		}
		select {
		case frames <- ScreencastFrame{Data: data}:
		default:
			// Drop rather than block the CDP event loop. The consumer samples
			// at its own cadence and only ever wants the newest frame.
		}
	})

	start := page.StartScreencast().
		WithFormat(page.ScreencastFormatJpeg).
		WithQuality(int64(quality)).
		WithEveryNthFrame(int64(every))
	if opts.MaxWidth > 0 {
		start = start.WithMaxWidth(int64(opts.MaxWidth))
	}
	if opts.MaxHeight > 0 {
		start = start.WithMaxHeight(int64(opts.MaxHeight))
	}
	if err := chromedp.Run(tabCtx, start); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("start screencast: %w", err)
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// Stop on tabCtx while it is still live, then release it.
			stopCtx, stopCancel := context.WithTimeout(tabCtx, m.timeout)
			_ = chromedp.Run(stopCtx, page.StopScreencast())
			stopCancel()
			cancel()
			close(frames)
		})
	}
	return frames, stop, nil
}
