package approval

import "context"

type requestIDKey struct{}
type executionKey struct{}

// WithExecution marks only a locally consumed approval for this call.
func WithExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, executionKey{}, true)
}

// IsExecution reports whether the local gate consumed approval for this call.
func IsExecution(ctx context.Context) bool {
	approved, _ := ctx.Value(executionKey{}).(bool)
	return approved
}

// WithRequestID carries an unprivileged request identifier across a proxy hop.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the pending request identifier, never operator authority.
func RequestID(ctx context.Context) string { id, _ := ctx.Value(requestIDKey{}).(string); return id }
