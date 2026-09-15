package browser

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// mergeCrossOriginFrames is the direct-CDP half of include_frames.
//
// A cross-origin iframe that Chrome put in its own process has a CDP target of
// its own, so brw attaches a session to it and runs the SAME walker there as on
// the top document — the controls come back as real refs (f<i>:<inner>) that
// resolve, not as coordinates to aim at. A cross-origin iframe that shares the
// embedder's process (same site, different port) has no target, so it keeps the
// existing treatment: the frame itself is promoted to a clickable box.
func (m *Manager) mergeCrossOriginFrames(ctx, tabCtx context.Context, snap *snapshot.PageSnapshot, opts snapshot.SnapshotOptions) {
	// Reading a frame's document is a read of a THIRD PARTY's site. The snapshot
	// was authorized against the origin the tab is showing, which is not a grant
	// to read the payment form or the editor that origin embeds, so each frame
	// origin is asked for on its own.
	frames, err := snapshot.SnapshotOutOfProcessFrames(tabCtx, opts, FrameReadCheckFromContext(ctx))
	read := map[int]bool{}
	if err == nil && len(frames) > 0 {
		_, read = snapshot.MergeOutOfProcessFrames(snap, frames)
	}
	// Every frame the session path could not read still gets a box the agent can
	// aim at, so include_frames never leaves a frame unmentioned.
	snapshot.PromoteCrossOriginFrames(snap, read)
}

// crossOriginActionableTimeoutMS is the actionability budget for a click inside a
// cross-origin frame. It matches Manager.Click's own 5s so the two paths wait the
// same amount for the same conditions.
const crossOriginActionableTimeoutMS = 5000

// clickCrossOriginFrameRef clicks an element inside an out-of-process iframe.
//
// The ref is resolved through a session attached to that frame's own target,
// gated on the same actionability script the ordinary click path runs (evaluated
// in the frame), and the box it returns is translated into top-level viewport
// coordinates, which is the only space CDP input speaks — refused outright when
// that point is not on screen inside the frame. The dispatch is a real browser
// gesture rather than an in-page MouseEvent: the in-page fast path builds its
// event in the TOP document, where the element does not exist.
func (m *Manager) clickCrossOriginFrameRef(ctx context.Context, ref string) (ActionResult, error) {
	if err := m.guardTakeover("click"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "click")

	box, err := snapshot.ResolveCrossOriginActionPoint(tabCtx, ref, crossOriginActionableTimeoutMS)
	if err != nil {
		return ActionResult{}, err
	}
	before := m.cachedBefore(tabID, tabCtx)
	modifiers := input.Modifier(m.heldModifierMask(tabID))
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelay, func() error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return dispatchClick(ctx, box.ViewportX, box.ViewportY, input.Left, 1, modifiers)
		}))
	}); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "clicked "+ref, before)
	appendWarning(&result, "clicked inside a cross-origin iframe by coordinate; the post-action observation reads the TOP document, so it cannot report what changed inside the frame")
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(tabID, TraceEntry{
		Action:     "click",
		Ref:        ref,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}
