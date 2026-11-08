package rpc

import (
	"context"
	"testing"

	"connectrpc.com/authn"
	"github.com/pug-sh/pug/internal/correlation"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "normal public key", key: "pub_abc123xyz", want: "...3xyz"},
		{name: "normal private key", key: "prv_def456uvw", want: "...6uvw"},
		{name: "exactly 5 chars", key: "abcde", want: "...bcde"},
		{name: "exactly 4 chars", key: "abcd", want: "***"},
		{name: "short key", key: "ab", want: "***"},
		{name: "empty string", key: "", want: "***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskKey(tt.key)
			if got != tt.want {
				t.Errorf("maskKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// spanAttrs returns the span attributes as a map for easy assertion.
func spanAttrs(span tracetest.SpanStub) map[string]string {
	m := make(map[string]string)
	for _, a := range span.Attributes {
		m[string(a.Key)] = a.Value.AsString()
	}
	return m
}

// enrichedSpan runs PrincipalInterceptor's enrichment for a project-scoped RPC
// with the given principal in context.
func enrichedSpan(t *testing.T, principal *Principal) tracetest.SpanStub {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("failed to shutdown tracer provider: %v", err)
		}
	}()

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	if principal != nil {
		ctx = authn.SetInfo(ctx, principal)
	}

	enrichSpanWithPrincipal(ctx, profilesv1connect.ProfilesServiceGetProcedure)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	return spans[0]
}

func TestPrincipalInterceptor_NoAuth(t *testing.T) {
	span := enrichedSpan(t, nil)
	attrs := spanAttrs(span)
	if _, ok := attrs["auth.type"]; ok {
		t.Error("expected no auth.type attribute on unauthenticated request")
	}
}

func TestPrincipalInterceptor_JWTAuth(t *testing.T) {
	principal := &Principal{
		AuthType: AuthTypeJWT,
		Customer: &dbread.Customer{ID: "cust-1"},
		Project:  &dbread.Project{ID: "proj-1", OrgID: "org-1"},
		JWTID:    "jti-abc123",
	}
	span := enrichedSpan(t, principal)
	attrs := spanAttrs(span)

	if got := attrs["auth.type"]; got != "jwt" {
		t.Errorf("auth.type = %q, want %q", got, "jwt")
	}
	if got := attrs["auth.jti"]; got != "jti-abc123" {
		t.Errorf("auth.jti = %q, want %q", got, "jti-abc123")
	}
	if got := attrs["customer.id"]; got != "cust-1" {
		t.Errorf("customer.id = %q, want %q", got, "cust-1")
	}
	if got := attrs["project.id"]; got != "proj-1" {
		t.Errorf("project.id = %q, want %q", got, "proj-1")
	}
	if got := attrs["org.id"]; got != "org-1" {
		t.Errorf("org.id = %q, want %q", got, "org-1")
	}
	if _, ok := attrs["auth.key_id"]; ok {
		t.Error("expected no auth.key_id attribute for JWT auth")
	}
}

func TestPrincipalInterceptor_APIKeyAuth(t *testing.T) {
	principal := &Principal{
		AuthType:     AuthTypePrivateKey,
		Project:      &dbread.Project{ID: "proj-2", OrgID: "org-2"},
		MaskedAPIKey: "...d456",
	}
	span := enrichedSpan(t, principal)
	attrs := spanAttrs(span)

	if got := attrs["auth.type"]; got != "priv_key" {
		t.Errorf("auth.type = %q, want %q", got, "priv_key")
	}
	if got := attrs["auth.key_id"]; got != "...d456" {
		t.Errorf("auth.key_id = %q, want %q", got, "...d456")
	}
	if got := attrs["project.id"]; got != "proj-2" {
		t.Errorf("project.id = %q, want %q", got, "proj-2")
	}
	if got := attrs["org.id"]; got != "org-2" {
		t.Errorf("org.id = %q, want %q", got, "org-2")
	}
	if _, ok := attrs["customer.id"]; ok {
		t.Error("expected no customer.id attribute for API key auth")
	}
	if _, ok := attrs["auth.jti"]; ok {
		t.Error("expected no auth.jti attribute for API key auth")
	}
}

func TestPrincipalInterceptor_MCPTokenAuth(t *testing.T) {
	attrs := spanAttrs(enrichedSpan(t, &Principal{
		AuthType: AuthTypeMCPToken,
		Customer: &dbread.Customer{ID: "cust-3"},
		Project:  &dbread.Project{ID: "proj-3", OrgID: "org-3"},
		GrantID:  "grant-3",
	}))
	for key, want := range map[string]string{
		"auth.type": "mcp_token", "auth.grant_id": "grant-3", "customer.id": "cust-3", "project.id": "proj-3", "org.id": "org-3",
	} {
		if got := attrs[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestPrincipalInterceptor_JWTWithoutProject(t *testing.T) {
	principal := &Principal{
		AuthType: AuthTypeJWT,
		Customer: &dbread.Customer{ID: "cust-1"},
		JWTID:    "jti-xyz",
	}
	span := enrichedSpan(t, principal)
	attrs := spanAttrs(span)

	if got := attrs["auth.type"]; got != "jwt" {
		t.Errorf("auth.type = %q, want %q", got, "jwt")
	}
	if got := attrs["customer.id"]; got != "cust-1" {
		t.Errorf("customer.id = %q, want %q", got, "cust-1")
	}
	if _, ok := attrs["project.id"]; ok {
		t.Error("expected no project.id when Project is nil")
	}
	if _, ok := attrs["org.id"]; ok {
		t.Error("expected no org.id when Project is nil")
	}
}

func TestPrincipalInterceptor_SetsTenant(t *testing.T) {
	projectTenant := correlation.Tenant{OrgID: "org-1", ProjectID: "proj-1"}
	tests := []struct {
		name      string
		ctx       context.Context
		procedure string
		want      correlation.Tenant
	}{
		{name: "api key", ctx: apiKeyCtx(), procedure: profilesv1connect.ProfilesServiceGetProcedure, want: projectTenant},
		{name: "jwt with project", ctx: jwtProjectCtx(), procedure: profilesv1connect.ProfilesServiceGetProcedure, want: projectTenant},
		{name: "jwt with project on an org-scoped RPC", ctx: jwtProjectCtx(), procedure: orgsv1connect.OrgsServiceListMembersProcedure},
		{name: "jwt with project on a self-service RPC", ctx: jwtProjectCtx(), procedure: orgsv1connect.OrgsServiceLeaveProcedure},
		{name: "jwt without project", ctx: customerCtx(), procedure: profilesv1connect.ProfilesServiceGetProcedure},
		{name: "no principal", ctx: context.Background(), procedure: profilesv1connect.ProfilesServiceGetProcedure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := correlation.WithTenant(tt.ctx)
			enrichSpanWithPrincipal(ctx, tt.procedure)
			if got := correlation.TenantFromContext(ctx); got != tt.want {
				t.Errorf("tenant = %+v, want %+v", got, tt.want)
			}
		})
	}
}
