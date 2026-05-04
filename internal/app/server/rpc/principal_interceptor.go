package rpc

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PrincipalInterceptor enriches the active OTel span with attributes from the
// authenticated Principal. It must be registered after the OTel interceptor
// (so a span exists) and requires the authn middleware to have already placed
// a Principal in the context.
func PrincipalInterceptor() connect.Interceptor {
	return &principalInterceptor{}
}

type principalInterceptor struct{}

func (i *principalInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		enrichSpanWithPrincipal(ctx)
		return next(ctx, req)
	}
}

func (i *principalInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *principalInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		enrichSpanWithPrincipal(ctx)
		return next(ctx, conn)
	}
}

func enrichSpanWithPrincipal(ctx context.Context) {
	// getPrincipalFromContext is called directly here (not via MustGet* extractors)
	// because the interceptor must gracefully handle unauthenticated requests (public endpoints).
	principal, err := getPrincipalFromContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "principal not in context, skipping span enrichment", slogx.Error(err))
		return
	}

	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("auth.type", string(principal.AuthType)),
	}

	if principal.JWTID != "" {
		attrs = append(attrs, attribute.String("auth.jti", principal.JWTID))
	}
	if principal.MaskedAPIKey != "" {
		attrs = append(attrs, attribute.String("auth.key_id", principal.MaskedAPIKey))
	}
	if principal.Customer != nil {
		attrs = append(attrs, attribute.String("customer.id", principal.Customer.ID))
	}
	if principal.Project != nil {
		attrs = append(attrs, attribute.String("project.id", principal.Project.ID))
		attrs = append(attrs, attribute.String("org.id", principal.Project.OrgID))
	}

	span.SetAttributes(attrs...)
}
