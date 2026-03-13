package rpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/slogx"
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

			// Let Connect RPC map context errors to the correct codes
			if ctx.Err() != nil {
				return resp, ctx.Err()
			}

			// Non-connect error - log and sanitize
			slog.ErrorContext(ctx, "unhandled error",
				slog.String("procedure", req.Spec().Procedure),
				slogx.Error(err))
			return resp, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
	}
}
