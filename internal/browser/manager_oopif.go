package browser

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

func (m *Manager) mergeCrossOriginFrames(ctx, tabCtx context.Context, snap *snapshot.PageSnapshot, opts snapshot.SnapshotOptions) {

	frames, err := snapshot.SnapshotOutOfProcessFrames(tabCtx, opts, FrameReadCheckFromContext(ctx))
	read := map[int]bool{}
	if err == nil && len(frames) > 0 {
		_, read = snapshot.MergeOutOfProcessFrames(snap, frames)
	}

	snapshot.PromoteCrossOriginFrames(snap, read)
}

const crossOriginActionableTimeoutMS = 5000

func (m *Manager) clickCrossOriginFrameRef(ctx context.Context, ref string) (ActionResult, error) {
	if err := m.guardTakeover("click"); err != nil {
		return ActionResult{}, err
	}
	actCheck := FrameActCheckFromContext(ctx)
	if actCheck == nil && FrameReadCheckFromContext(ctx) != nil {
		return ActionResult{}, ErrFrameActCheckMissing
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "click")

	box, err := snapshot.ResolveCrossOriginActionPoint(tabCtx, ref, crossOriginActionableTimeoutMS, actCheck)
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
