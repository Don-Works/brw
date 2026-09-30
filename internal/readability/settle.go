package readability

import "context"

type settleKey struct{}

// WithSettleMS carries a validated read settle budget to a browser controller.
func WithSettleMS(ctx context.Context, ms int) context.Context {
	return context.WithValue(ctx, settleKey{}, ms)
}

// SettleMS returns the requested budget, or the default when none was supplied.
func SettleMS(ctx context.Context) int {
	if ms, ok := ctx.Value(settleKey{}).(int); ok {
		return ms
	}
	return readSettleCapMS
}
