package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
)

// ConnectCtxErr wraps a context error in the appropriate Connect error code
// with an explicit client-facing message. Use at handler entry to short-circuit
// when ctx.Err() is non-nil. The errorInterceptor in error.go would otherwise
// pass the raw ctx error through and let Connect map the code, but without an
// explicit message. Returns nil for a nil err so call sites can keep the
// "if err := ctx.Err(); err != nil" idiom without re-checking.
func ConnectCtxErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request timed out"))
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	// Defensive: only context errors should reach this path. Anything else is
	// a programmer error (passing a non-context error). Return Internal so the
	// mistake surfaces as a server bug rather than a misleading "canceled".
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
