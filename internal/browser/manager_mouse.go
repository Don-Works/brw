package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Generic coordinate computer-action family. These primitives are pure web
// standards: CDP Input.dispatchMouseEvent on the direct-CDP transport. No
// site-specific logic. Refs resolve through snapshot.ResolveOrRecoverBox so
// iframe coordinate translation + scroll-into-view apply, exactly like the
// normal click path, and every action emits a post-action observation.

const defaultDragSteps = 12

// normalizeMouseButton maps a user-supplied button name to a CDP MouseButton,
// defaulting to left. Generic over standard pointer buttons only.
func normalizeMouseButton(button string) input.MouseButton {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		return input.Left
	case "right":
		return input.Right
	case "middle":
		return input.Middle
	case "back":
		return input.Back
	case "forward":
		return input.Forward
	case "none":
		return input.None
	default:
		return input.Left
	}
}

// buttonsMask returns the bitfield value identifying which button is held while
// a button is pressed (Left=1, Right=2, Middle=4, Back=8, Forward=16, None=0).
func buttonsMask(button input.MouseButton) int64 {
	switch button {
	case input.Left:
		return 1
	case input.Right:
		return 2
	case input.Middle:
		return 4
	case input.Back:
		return 8
	case input.Forward:
		return 16
	default:
		return 0
	}
}

// resolvePoint returns the viewport coordinates for a MousePoint. A ref is
// resolved (and recovered if stale) through ResolveOrRecoverBox so scroll into
// view + iframe translation apply; otherwise explicit x,y are used verbatim.
// The returned recovery note feeds the observation warning and trace.
func resolvePoint(tabCtx context.Context, point MousePoint) (x, y float64, recovery string, err error) {
	if point.HasRef() {
		box, rerr := snapshot.ResolveOrRecoverBox(tabCtx, point.Ref)
		if rerr != nil {
			return 0, 0, "", rerr
		}
		if box.Recovered {
			recovery = fmt.Sprintf("ref recovered: %s -> %s", box.OldRef, box.Ref)
		}
		return box.ViewportX, box.ViewportY, recovery, nil
	}
	if point.HasXY() {
		return *point.X, *point.Y, "", nil
	}
	return 0, 0, "", errors.New("mouse target requires either a ref or x and y coordinates")
}

// pointDescriptor returns a compact human label for a MousePoint, used in
// observation messages.
func pointDescriptor(point MousePoint) string {
	if point.HasRef() {
		return point.Ref
	}
	if point.HasXY() {
		return fmt.Sprintf("(%.0f,%.0f)", *point.X, *point.Y)
	}
	return "?"
}

// ClickButton performs a single click at a ref or x,y with an explicit mouse
// button and click count: right-click opens context menus, click_count:2 is a
// double-click, click_count:3 selects a line, button:middle is a middle-click.
func (m *Manager) ClickButton(ctx context.Context, opts ClickButtonOptions) (ActionResult, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	if opts.HasRef() {
		if err := snapshot.WaitForActionable(tabCtx, opts.Ref, 5000); err != nil {
			return ActionResult{}, err
		}
	}
	x, y, recovery, err := resolvePoint(tabCtx, opts.MousePoint)
	if err != nil {
		return ActionResult{}, err
	}
	button := normalizeMouseButton(opts.Button)
	clickCount := opts.ClickCount
	if clickCount <= 0 {
		clickCount = 1
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return dispatchClick(ctx, x, y, button, clickCount)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)

	desc := fmt.Sprintf("%s-clicked %s", buttonLabel(button), pointDescriptor(opts.MousePoint))
	if clickCount > 1 {
		desc = fmt.Sprintf("%s-clicked x%d %s", buttonLabel(button), clickCount, pointDescriptor(opts.MousePoint))
	}
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	if recovery != "" {
		appendWarning(&result, recovery)
	}
	m.recordTrace(TraceEntry{
		Action:     "click_button",
		Ref:        opts.Ref,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// MouseDown presses (and holds) a mouse button at a ref or x,y without
// releasing — the press half of a press-and-hold. Pair with MouseUp.
func (m *Manager) MouseDown(ctx context.Context, opts MouseButtonOptions) (ActionResult, error) {
	return m.mouseHalf(ctx, opts, input.MousePressed, "mouse_down")
}

// MouseUp releases a held mouse button at a ref or x,y — the release half of a
// press-and-hold. Pair with MouseDown.
func (m *Manager) MouseUp(ctx context.Context, opts MouseButtonOptions) (ActionResult, error) {
	return m.mouseHalf(ctx, opts, input.MouseReleased, "mouse_up")
}

func (m *Manager) mouseHalf(ctx context.Context, opts MouseButtonOptions, eventType input.MouseType, action string) (ActionResult, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	if opts.HasRef() {
		if err := snapshot.WaitForActionable(tabCtx, opts.Ref, 5000); err != nil {
			return ActionResult{}, err
		}
	}
	x, y, recovery, err := resolvePoint(tabCtx, opts.MousePoint)
	if err != nil {
		return ActionResult{}, err
	}
	button := normalizeMouseButton(opts.Button)
	buttons := int64(0)
	if eventType == input.MousePressed {
		buttons = buttonsMask(button)
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(eventType, x, y).
			WithButton(button).
			WithButtons(buttons).
			WithClickCount(1).
			Do(ctx)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, mouseHalfSettleDelay)

	desc := fmt.Sprintf("%s %s at %s", action, buttonLabel(button), pointDescriptor(opts.MousePoint))
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	if recovery != "" {
		appendWarning(&result, recovery)
	}
	m.recordTrace(TraceEntry{
		Action:     action,
		Ref:        opts.Ref,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// Drag presses at a source point, moves to a target point over several
// intermediate steps, then releases — covering sliders/range inputs,
// drag-and-drop reorder, and canvas/map panning. Pure CDP Input domain.
func (m *Manager) Drag(ctx context.Context, opts DragOptions) (ActionResult, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	if opts.From.HasRef() {
		if err := snapshot.WaitForActionable(tabCtx, opts.From.Ref, 5000); err != nil {
			return ActionResult{}, err
		}
	}
	// Native HTML5 drag-and-drop (draggable=true source) is NOT driven by CDP
	// mouse events — Chromium only synthesises drag events for a real OS drag, so a
	// coordinate drag silently no-ops on these widgets. Dispatch the real HTML5
	// drag-event sequence between the two refs first; if the target accepts the
	// drop we are done, otherwise fall through to the coordinate drag (which covers
	// pointer-based libraries like jQuery UI sortable that listen on mousedown).
	if opts.From.HasRef() && opts.To.HasRef() && snapshot.RefDraggable(tabCtx, opts.From.Ref) {
		before := m.cachedBefore(tabID, tabCtx)
		dropped, dErr := snapshot.DragHtml5(tabCtx, opts.From.Ref, opts.To.Ref)
		if dErr == nil && dropped {
			m.settle(tabCtx, actionSettleDelay)
			result := m.observeActionWithBefore(tabID, tabCtx, fmt.Sprintf("dragged %s -> %s (html5)", opts.From.Ref, opts.To.Ref), before)
			result.DurationMS = time.Since(start).Milliseconds()
			m.recordTrace(TraceEntry{
				Action:     "drag",
				Ref:        opts.From.Ref,
				OK:         result.OK,
				DurationMS: result.DurationMS,
				Timestamp:  time.Now().Format(time.RFC3339),
			})
			return result, nil
		}
	}
	fromX, fromY, recovery, err := resolvePoint(tabCtx, opts.From)
	if err != nil {
		return ActionResult{}, fmt.Errorf("drag source: %w", err)
	}
	// Resolve the target after the source so a ref-based target reflects any
	// layout change caused by scrolling the source into view.
	toX, toY, toRecovery, err := resolvePoint(tabCtx, opts.To)
	if err != nil {
		return ActionResult{}, fmt.Errorf("drag target: %w", err)
	}
	if recovery == "" {
		recovery = toRecovery
	}
	button := normalizeMouseButton(opts.Button)
	steps := opts.Steps
	if steps <= 0 {
		steps = defaultDragSteps
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return dispatchDrag(ctx, fromX, fromY, toX, toY, steps, button)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)

	desc := fmt.Sprintf("dragged %s -> %s", pointDescriptor(opts.From), pointDescriptor(opts.To))
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	if recovery != "" {
		appendWarning(&result, recovery)
	}
	m.recordTrace(TraceEntry{
		Action:     "drag",
		Ref:        opts.From.Ref,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// dispatchClick presses then releases at (x,y) with the given button and click
// count. A double/triple click is a single dispatch with clickCount 2/3, which
// is how Chromium itself models repeated clicks.
func dispatchClick(ctx context.Context, x, y float64, button input.MouseButton, clickCount int) error {
	buttons := buttonsMask(button)
	if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
		return err
	}
	if err := input.DispatchMouseEvent(input.MousePressed, x, y).
		WithButton(button).
		WithButtons(buttons).
		WithClickCount(int64(clickCount)).
		Do(ctx); err != nil {
		return err
	}
	return input.DispatchMouseEvent(input.MouseReleased, x, y).
		WithButton(button).
		WithButtons(0).
		WithClickCount(int64(clickCount)).
		Do(ctx)
}

// dispatchDrag emits mousePressed at the source, a series of mouseMoved events
// interpolated toward the target (with the button held in the buttons mask),
// then mouseReleased at the target.
func dispatchDrag(ctx context.Context, fromX, fromY, toX, toY float64, steps int, button input.MouseButton) error {
	buttons := buttonsMask(button)
	if err := input.DispatchMouseEvent(input.MouseMoved, fromX, fromY).Do(ctx); err != nil {
		return err
	}
	if err := input.DispatchMouseEvent(input.MousePressed, fromX, fromY).
		WithButton(button).
		WithButtons(buttons).
		WithClickCount(1).
		Do(ctx); err != nil {
		return err
	}
	if steps < 1 {
		steps = 1
	}
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		mx := fromX + (toX-fromX)*t
		my := fromY + (toY-fromY)*t
		if err := input.DispatchMouseEvent(input.MouseMoved, mx, my).
			WithButton(button).
			WithButtons(buttons).
			Do(ctx); err != nil {
			return err
		}
	}
	return input.DispatchMouseEvent(input.MouseReleased, toX, toY).
		WithButton(button).
		WithButtons(0).
		WithClickCount(1).
		Do(ctx)
}

func buttonLabel(button input.MouseButton) string {
	s := string(button)
	if s == "" {
		return "left"
	}
	return s
}
