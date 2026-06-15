package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// Coordinate computer-action family for the extension bridge. The bridge has no
// direct CDP Input access, so these actuate through in-page pointer/mouse event
// sequences (the same standards-only approach the bridge already uses for
// clickRef/ClickXY). Each action emits a post-action observation, mirroring the
// direct-CDP Manager path.

func mousePointArg(point browser.MousePoint) map[string]any {
	arg := map[string]any{}
	if point.HasRef() {
		arg["ref"] = point.Ref
	}
	if point.HasXY() {
		arg["x"] = *point.X
		arg["y"] = *point.Y
	}
	return arg
}

func (b *Bridge) ClickButton(ctx context.Context, opts browser.ClickButtonOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	arg := mousePointArg(opts.MousePoint)
	if opts.Button != "" {
		arg["button"] = opts.Button
	}
	clickCount := opts.ClickCount
	if clickCount <= 0 {
		clickCount = 1
	}
	arg["click_count"] = clickCount

	if _, err := b.dispatchMouse(ctx, snapshot.MouseEventScript, arg); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)

	desc := fmt.Sprintf("%s-clicked %s", buttonLabel(opts.Button), pointDescriptor(opts.MousePoint))
	if clickCount > 1 {
		desc = fmt.Sprintf("%s-clicked x%d %s", buttonLabel(opts.Button), clickCount, pointDescriptor(opts.MousePoint))
	}
	return b.observeActionWithBefore(ctx, desc, before), nil
}

func (b *Bridge) MouseDown(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	return b.mouseHalf(ctx, opts, "down", "mouse_down")
}

func (b *Bridge) MouseUp(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	return b.mouseHalf(ctx, opts, "up", "mouse_up")
}

func (b *Bridge) mouseHalf(ctx context.Context, opts browser.MouseButtonOptions, phase, action string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	arg := mousePointArg(opts.MousePoint)
	if opts.Button != "" {
		arg["button"] = opts.Button
	}
	arg["phase"] = phase

	if _, err := b.dispatchMouse(ctx, snapshot.MouseHalfScript, arg); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)

	desc := fmt.Sprintf("%s %s at %s", action, buttonLabel(opts.Button), pointDescriptor(opts.MousePoint))
	return b.observeActionWithBefore(ctx, desc, before), nil
}

func (b *Bridge) Drag(ctx context.Context, opts browser.DragOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	arg := map[string]any{
		"from": mousePointArg(opts.From),
		"to":   mousePointArg(opts.To),
	}
	if opts.Button != "" {
		arg["button"] = opts.Button
	}
	if opts.Steps > 0 {
		arg["steps"] = opts.Steps
	}

	if _, err := b.dispatchMouse(ctx, snapshot.DragScript, arg); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)

	desc := fmt.Sprintf("dragged %s -> %s", pointDescriptor(opts.From), pointDescriptor(opts.To))
	return b.observeActionWithBefore(ctx, desc, before), nil
}

func (b *Bridge) dispatchMouse(ctx context.Context, script string, arg map[string]any) (snapshot.MouseActionResult, error) {
	argJSON, _ := json.Marshal(arg)
	var result snapshot.MouseActionResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", script, argJSON), "", &result); err != nil {
		return snapshot.MouseActionResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "mouse action failed"
		}
		return result, errors.New(result.Error)
	}
	return result, nil
}

func buttonLabel(button string) string {
	b := strings.ToLower(strings.TrimSpace(button))
	if b == "" {
		return "left"
	}
	return b
}

func pointDescriptor(point browser.MousePoint) string {
	if point.HasRef() {
		return point.Ref
	}
	if point.HasXY() {
		return fmt.Sprintf("(%.0f,%.0f)", *point.X, *point.Y)
	}
	return "?"
}
