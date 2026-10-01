package browser

import (
	"context"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/input"
	"time"
)

func (m *Manager) emulatedTouchEnabled(ctx context.Context) bool {
	m.emulationMu.Lock()
	state := m.emulationStates[eventScopeFromCtx(ctx)]
	m.emulationMu.Unlock()
	return state.Config.Touch
}

func dispatchTrustedTap(ctx context.Context, x, y float64, modifiers input.Modifier) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer cancel()
		if cleanupErr := input.DispatchTouchEvent(input.TouchCancel, []*input.TouchPoint{}).WithModifiers(modifiers).Do(cleanup); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("touch cancellation unconfirmed: %w", cleanupErr))
		}
	}()
	if err = input.DispatchTouchEvent(input.TouchStart, []*input.TouchPoint{{X: x, Y: y, ID: 1}}).WithModifiers(modifiers).Do(ctx); err != nil {
		return err
	}
	return input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).WithModifiers(modifiers).Do(ctx)
}
