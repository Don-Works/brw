package browser

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Touch synthesizes a tap or a swipe through the DevTools Protocol touch input
// channel. It is the gesture primitive for mobile flows: brw_emulate_device sets
// the metrics and touch capability, and this is what actually taps and swipes.
func (m *Manager) Touch(ctx context.Context, opts TouchOptions) (ActionResult, error) {
	req, err := NormalizeTouch(opts)
	if err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("touch"); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("touch", GenericCrossOriginRemedy, req.Ref, req.ToRef); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	fromX, fromY, fromRecovery, err := resolvePoint(tabCtx, MousePoint{Ref: req.Ref, X: req.X, Y: req.Y})
	if err != nil {
		return ActionResult{}, fmt.Errorf("resolve touch start: %w", err)
	}
	toX, toY, toRecovery := fromX, fromY, fromRecovery
	if req.Action == "swipe" {
		toX, toY, toRecovery, err = resolvePoint(tabCtx, MousePoint{Ref: req.ToRef, X: req.ToX, Y: req.ToY})
		if err != nil {
			return ActionResult{}, fmt.Errorf("resolve touch end: %w", err)
		}
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := m.runWithPrearmedSettle(tabCtx, mouseHalfSettleDelay, func() error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return dispatchTouchGesture(runCtx, req, fromX, fromY, toX, toY)
		}))
	}); err != nil {
		return ActionResult{}, err
	}

	desc := touchDescriptor(req, fromX, fromY, toX, toY)
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	if fromRecovery != "" {
		appendWarning(&result, fromRecovery)
	}
	if toRecovery != "" {
		appendWarning(&result, toRecovery)
	}
	m.recordTrace(tabID, TraceEntry{
		Action:     "touch",
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// dispatchTouchGesture runs one gesture, repeated. A tap is start+end at one
// point; a swipe interpolates touchMove steps and releases at the end point.
// TouchEnd carries no touch points, which CDP requires.
func dispatchTouchGesture(ctx context.Context, req TouchOptions, fromX, fromY, toX, toY float64) error {
	for i := 0; i < req.Repeat; i++ {
		point := func(x, y float64) []*input.TouchPoint {
			return []*input.TouchPoint{{X: x, Y: y, ID: 1}}
		}
		if err := input.DispatchTouchEvent(input.TouchStart, point(fromX, fromY)).Do(ctx); err != nil {
			return err
		}
		if req.Action == "swipe" {
			steps := touchMoveSteps
			if steps < 1 {
				steps = 1
			}
			stepDelay := time.Duration(req.DurationMS/steps) * time.Millisecond
			for s := 1; s <= steps; s++ {
				frac := float64(s) / float64(steps)
				x := fromX + (toX-fromX)*frac
				y := fromY + (toY-fromY)*frac
				if err := input.DispatchTouchEvent(input.TouchMove, point(x, y)).Do(ctx); err != nil {
					return err
				}
				if s < steps && stepDelay > 0 {
					time.Sleep(stepDelay)
				}
			}
		}
		if err := input.DispatchTouchEvent(input.TouchEnd, nil).Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

func touchDescriptor(req TouchOptions, fromX, fromY, toX, toY float64) string {
	at := fmt.Sprintf("(%.0f,%.0f)", fromX, fromY)
	if req.Action == "tap" {
		if req.Ref != "" {
			at = req.Ref
		}
		if req.Repeat > 1 {
			return fmt.Sprintf("tapped %s %d times", at, req.Repeat)
		}
		return fmt.Sprintf("tapped %s", at)
	}
	end := fmt.Sprintf("(%.0f,%.0f)", toX, toY)
	if req.ToRef != "" {
		end = req.ToRef
	}
	desc := fmt.Sprintf("swiped %s -> %s over %dms", at, end, req.DurationMS)
	if req.Repeat > 1 {
		desc += fmt.Sprintf(" x%d", req.Repeat)
	}
	return desc
}
