package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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

// ScreencastFrame is one JPEG the compositor produced, with the metadata Chrome
// swapped it with.
//
// Timestamp is Chrome's frame-swap time — when the page repainted, not when brw
// read the event — and is ZERO when Chrome sent no usable one. The two are
// different clocks and only one of them orders the stream, which is why the
// fallback is a zero value a consumer can test rather than our own clock reading
// dressed up as the compositor's. Width and Height are the page's own DIP
// viewport, which is what makes a frame addressable: a viewer scaling the JPEG
// into a smaller element can map a click back onto page coordinates only if it
// knows what the frame covers.
type ScreencastFrame struct {
	Data      []byte
	Timestamp time.Time
	Width     float64
	Height    float64
	ScrollX   float64
	ScrollY   float64
}

// screencastStats counts what one stream cost and what it discarded. Unexported
// on purpose: it measures a single run rather than describing the transport
// capability, and an exported type no consumer reads is API that only looks like
// a promise. Dropping is normal under backpressure and is the reason the encoder
// degrades framerate instead of stalling the compositor.
type screencastStats struct {
	Frames  int64
	Bytes   int64
	Dropped int64
	// OutOfOrder counts frames Chrome stamped at or before the previous frame's
	// swap. They are discarded: a consumer pacing on swap time must never see
	// the compositor's clock go backwards.
	OutOfOrder int64
	// UnstampedFrames counts frames delivered with no swap time brw could use —
	// no metadata, no timestamp in it, or one that is not behind the moment we
	// read the event. They are forwarded, but they set no ordering floor.
	UnstampedFrames int64
}

// screencastCounters is written from the CDP event goroutine and read by the
// caller, so every field is atomic rather than relying on the stop() happening
// before the read.
type screencastCounters struct {
	frames     atomic.Int64
	bytes      atomic.Int64
	dropped    atomic.Int64
	outOfOrder atomic.Int64
	unstamped  atomic.Int64
}

func (c *screencastCounters) snapshot() screencastStats {
	return screencastStats{
		Frames:          c.frames.Load(),
		Bytes:           c.bytes.Load(),
		Dropped:         c.dropped.Load(),
		OutOfOrder:      c.outOfOrder.Load(),
		UnstampedFrames: c.unstamped.Load(),
	}
}

// screencastAdmission is what the frame listener does with one frame once its
// swap time has been judged.
type screencastAdmission int

const (
	// frameInOrder: forward it; the ordering floor has advanced to its swap.
	frameInOrder screencastAdmission = iota
	// frameUnstamped: forward it, but the ordering floor is unchanged.
	frameUnstamped
	// frameOutOfOrder: discard it; the floor is already at or past its swap.
	frameOutOfOrder
)

// admitScreencastFrame places one frame in the stream and returns the swap time
// to publish with it, advancing lastSwap when it takes one.
//
// stamped is Chrome's frame-swap time and arrivedAt is the moment brw read the
// event carrying it. The two are different clocks. A swap is when the page
// repainted, so it is necessarily BEHIND the read; a value that is not is either
// a clock the browser process does not share with us or our own clock wearing
// the compositor's name. Such a value must never set the ordering floor — it
// would push the floor into the future and take every real frame behind it down
// with it — so the frame is forwarded unstamped instead. The CDP event stream is
// ordered, so a frame with no usable swap time is already in order; substituting
// our own clock is what would start discarding the frames behind it.
func admitScreencastFrame(lastSwap *atomic.Int64, stamped, arrivedAt time.Time) (screencastAdmission, time.Time) {
	if stamped.IsZero() || !stamped.Before(arrivedAt) {
		return frameUnstamped, time.Time{}
	}
	swap := stamped.UnixNano()
	for {
		previous := lastSwap.Load()
		if swap <= previous {
			return frameOutOfOrder, stamped
		}
		if lastSwap.CompareAndSwap(previous, swap) {
			return frameInOrder, stamped
		}
	}
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
// A Manager capability, so it holds on both DevTools Protocol lanes. The
// extension bridge has no equivalent, and callers fall back to the screenshot
// loop when a controller does not implement this.
func (m *Manager) ScreencastFrames(ctx context.Context, opts ScreencastOptions) (<-chan ScreencastFrame, func(), error) {
	frames, stop, _, err := m.screencastFrames(ctx, opts)
	return frames, stop, err
}

// screencastFrames is ScreencastFrames plus the counters. Kept unexported
// because the counts are a measurement of one run, not part of the transport
// capability every consumer has to implement.
func (m *Manager) screencastFrames(ctx context.Context, opts ScreencastOptions) (<-chan ScreencastFrame, func(), func() screencastStats, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, nil, nil, err
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
	counters := &screencastCounters{}
	// lastSwap is the frame-swap time of the newest frame forwarded. Chrome
	// delivers on a websocket and brw acks asynchronously, so ordering is not
	// free; anything at or before it is dropped rather than reordered.
	var lastSwap atomic.Int64
	// sendMu keeps the listener off a channel stop() has closed. The listener
	// runs on chromedp's event goroutine and stop() on the caller's, so without
	// it a frame arriving during teardown is a send on a closed channel.
	var sendMu sync.RWMutex
	closed := false

	// Register the listener BEFORE starting, so the first frame — the one an
	// otherwise-static page depends on — cannot arrive before we are listening.
	chromedp.ListenTarget(tabCtx, func(ev any) {
		e, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		// Taken before anything else in the handler, so it is genuinely "when brw
		// read the event" and can be used to sanity-check Chrome's own clock.
		arrivedAt := time.Now()
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
		frame := ScreencastFrame{Data: data}
		if e.Metadata != nil {
			frame.Width = e.Metadata.DeviceWidth
			frame.Height = e.Metadata.DeviceHeight
			frame.ScrollX = e.Metadata.ScrollOffsetX
			frame.ScrollY = e.Metadata.ScrollOffsetY
			if e.Metadata.Timestamp != nil {
				frame.Timestamp = time.Time(*e.Metadata.Timestamp)
			}
		}
		admission, swap := admitScreencastFrame(&lastSwap, frame.Timestamp, arrivedAt)
		frame.Timestamp = swap
		switch admission {
		case frameOutOfOrder:
			counters.outOfOrder.Add(1)
			return
		case frameUnstamped:
			counters.unstamped.Add(1)
		}
		sendMu.RLock()
		defer sendMu.RUnlock()
		if closed {
			return
		}
		select {
		case frames <- frame:
			counters.frames.Add(1)
			counters.bytes.Add(int64(len(data)))
		default:
			// Drop rather than block the CDP event loop. The consumer samples
			// at its own cadence and only ever wants the newest frame; a
			// dropped frame costs framerate and nothing else, because each
			// frame is a complete image rather than a delta on the last one.
			counters.dropped.Add(1)
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
		return nil, nil, nil, fmt.Errorf("start screencast: %w", err)
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// Stop on tabCtx while it is still live, then release it.
			stopCtx, stopCancel := context.WithTimeout(tabCtx, m.timeout)
			_ = chromedp.Run(stopCtx, page.StopScreencast())
			stopCancel()
			cancel()
			sendMu.Lock()
			closed = true
			close(frames)
			sendMu.Unlock()
		})
	}
	return frames, stop, counters.snapshot, nil
}
