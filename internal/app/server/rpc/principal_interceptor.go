package rpc

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PrincipalInterceptor enriches the active OTel span with attributes from the
// authenticated Principal, and makes its project the tenant of a project-scoped RPC.
// It must be registered after the OTel interceptor (so a span exists) and before
// validate (so a request that fails validation still names its tenant), and requires
// the authn middleware to have already placed a Principal in the context.
func PrincipalInterceptor() connect.Interceptor {
	return &principalInterceptor{}
}

type principalInterceptor struct{}

func (i *principalInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		enrichSpanWithPrincipal(ctx, req.Spec().Procedure)
		return next(ctx, req)
	}
}

func (i *principalInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *principalInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		enrichSpanWithPrincipal(ctx, conn.Spec().Procedure)
		return next(ctx, conn)
	}
}

func enrichSpanWithPrincipal(ctx context.Context, procedure string) {
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
	if principal.GrantID != "" {
		attrs = append(attrs, attribute.String("auth.grant_id", principal.GrantID))
	}
	if principal.Customer != nil {
		attrs = append(attrs, attribute.String("customer.id", principal.Customer.ID))
	}

	span.SetAttributes(attrs...)
	// An org-scoped RPC acts for its message's org, not an x-project-id it was sent.
	if principal.Project != nil && permissionRegistry[procedure].IsProjectScoped() {
		telemetry.SetTenant(ctx, principal.Project.OrgID, principal.Project.ID)
	}
}
