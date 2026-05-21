package rpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/correlation"
	"github.com/rs/xid"
)

// WithCorrelationID ensures a correlation id is in the request context before any
// handler OR middleware runs. Mount it OUTSIDE the authn middleware: auth
// rejections are written outside the Connect interceptor chain, so without this
// they would carry no id. CorrelationInterceptor reuses whatever id this sets.
func WithCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(ensureCorrelationID(r.Context())))
	})
}

// ensureCorrelationID returns a context carrying a correlation id, minting a new
// one only if none is already present so an upstream id (e.g. from
// WithCorrelationID) is preserved end to end.
func ensureCorrelationID(ctx context.Context) context.Context {
	if correlation.IDFromContext(ctx) != "" {
		return ctx
	}
	return correlation.WithID(ctx, xid.New().String())
}

// CorrelationInterceptor mints a per-request correlation id into the context when
// one is not already present. Register it FIRST so every downstream log line and
// error detail share the same id; when WithCorrelationID already set one (the
// normal path), this reuses it. Independent of OpenTelemetry, so the id always exists.
func CorrelationInterceptor() connect.Interceptor { return &correlationInterceptor{} }

type correlationInterceptor struct{}

func (i *correlationInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(ensureCorrelationID(ctx), req)
	}
}

func (i *correlationInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *correlationInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(ensureCorrelationID(ctx), conn)
	}
}
