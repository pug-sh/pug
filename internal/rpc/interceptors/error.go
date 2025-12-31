package interceptors

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

			if connectErr, ok := err.(*connect.Error); ok && connectErr.Code() == connect.CodeInternal {
				slog.ErrorContext(ctx, "internal error",
					slog.String("procedure", req.Spec().Procedure),
					slog.Any("error", connectErr.Unwrap()))
				return resp, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}

			return resp, err
		}
	}
}
