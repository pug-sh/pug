package rpc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
)

func ErrorInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err == nil {
				return resp, nil
			}

			var connectErr *connect.Error
			if errors.As(err, &connectErr) {
				return resp, connectErr
			}

			// Non-connect error - log and sanitize
			slog.ErrorContext(ctx, "unhandled error",
				slog.String("procedure", req.Spec().Procedure),
				slog.Any("error", err))
			return resp, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
	}
}
