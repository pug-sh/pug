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
// explicit message.
func ConnectCtxErr(err error) error {
	code := connect.CodeCanceled
	msg := "request canceled"
	if errors.Is(err, context.DeadlineExceeded) {
		code = connect.CodeDeadlineExceeded
		msg = "request timed out"
	}
	return connect.NewError(code, errors.New(msg))
}
