package rpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/slogx"
)

// LoggingInterceptor logs every RPC request. Since slog is bridged to OTel via
// otelslog, these log records are automatically exported to the OTel collector.
func LoggingInterceptor() connect.Interceptor {
	return &loggingInterceptor{}
}

type loggingInterceptor struct{}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		i.logUnary(ctx, req, err, start)
		return resp, err
	}
}

func (i *loggingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *loggingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		i.logStream(ctx, conn, err, start)
		return err
	}
}

func (i *loggingInterceptor) logUnary(ctx context.Context, req connect.AnyRequest, err error, start time.Time) {
	args := []any{
		slog.String("procedure", req.Spec().Procedure),
		slog.Duration("duration", time.Since(start)),
	}
	logRPC(ctx, err, args)
}

func (i *loggingInterceptor) logStream(ctx context.Context, conn connect.StreamingHandlerConn, err error, start time.Time) {
	args := []any{
		slog.String("procedure", conn.Spec().Procedure),
		slog.Duration("duration", time.Since(start)),
	}
	logRPC(ctx, err, args)
}

func logRPC(ctx context.Context, err error, args []any) {
	if err == nil {
		slog.InfoContext(ctx, "rpc ok", args...)
		return
	}
	args = append(args, slogx.Error(err))
	if isClientError(err) {
		slog.WarnContext(ctx, "rpc error", args...)
	} else {
		slog.ErrorContext(ctx, "rpc error", args...)
	}
}

func isClientError(err error) bool {
	code := connect.CodeOf(err)
	// A raw *apperr.Error (not yet rewritten by ErrorInterceptor) carries its code
	// in a field, so connect.CodeOf returns Unknown for it. Read the code directly
	// so classification is correct regardless of interceptor ordering.
	if ae, ok := errors.AsType[*apperr.Error](err); ok {
		code = ae.Code()
	}
	switch code {
	case connect.CodeInvalidArgument,
		connect.CodeNotFound,
		connect.CodeAlreadyExists,
		connect.CodePermissionDenied,
		connect.CodeUnauthenticated,
		connect.CodeFailedPrecondition,
		connect.CodeOutOfRange,
		connect.CodeCanceled:
		return true
	default:
		return false
	}
}
