package rpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/correlation"
	"github.com/rs/xid"
)

// CorrelationInterceptor mints a per-request correlation id into the context.
// Register it FIRST so every downstream log line and the error detail share
// the same id. Independent of OpenTelemetry, so the id always exists.
func CorrelationInterceptor() connect.Interceptor { return &correlationInterceptor{} }

type correlationInterceptor struct{}

func (i *correlationInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(correlation.WithID(ctx, xid.New().String()), req)
	}
}

func (i *correlationInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *correlationInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(correlation.WithID(ctx, xid.New().String()), conn)
	}
}
