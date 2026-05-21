// Package correlation carries a per-request correlation id through context.
// It is a leaf package so both the RPC layer (which mints the id) and the
// telemetry layer (which stamps it onto logs) can depend on it without a cycle.
package correlation

import "context"

type ctxKey struct{}

// WithID returns a context carrying the given correlation id.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IDFromContext returns the correlation id, or "" if none is set.
func IDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
