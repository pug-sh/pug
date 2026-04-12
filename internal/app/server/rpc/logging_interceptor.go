package rpc

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
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
	status := "ok"
	if err != nil {
		status = "error"
	}
	slog.InfoContext(ctx, status+" "+req.Spec().Procedure+" "+time.Since(start).String(),
		slog.String("procedure", req.Spec().Procedure),
		slog.Duration("duration", time.Since(start)),
		slog.Any("error", err),
	)
}

func (i *loggingInterceptor) logStream(ctx context.Context, conn connect.StreamingHandlerConn, err error, start time.Time) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	slog.InfoContext(ctx, status+" "+conn.Spec().Procedure+" "+time.Since(start).String(),
		slog.String("procedure", conn.Spec().Procedure),
		slog.Duration("duration", time.Since(start)),
		slog.Any("error", err),
	)
}
