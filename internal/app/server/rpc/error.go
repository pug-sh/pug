package rpc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/slogx"
)

func ErrorInterceptor() connect.Interceptor {
	return &errorInterceptor{}
}

type errorInterceptor struct{}

func (i *errorInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		return resp, sanitizeError(ctx, req.Spec().Procedure, err)
	}
}

func (i *errorInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *errorInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		return sanitizeError(ctx, conn.Spec().Procedure, err)
	}
}

func sanitizeError(ctx context.Context, procedure string, err error) error {
	if err == nil {
		return nil
	}

	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return connectErr
	}

	// Let Connect RPC map context errors to the correct codes
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Non-connect error - log and sanitize
	slog.ErrorContext(ctx, "unhandled error",
		slog.String("procedure", procedure),
		slogx.Error(err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
