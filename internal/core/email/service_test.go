package email_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
)

type captureProvider struct {
	got coreemail.Message
}

func (p *captureProvider) Send(_ context.Context, msg coreemail.Message) error {
	p.got = msg
	return nil
}

type staticResolver struct {
	provider  coreemail.Provider
	from      coreemail.ResolvedFrom
	gotTenant *string
}

func (r *staticResolver) Resolve(_ context.Context, tenantID *string) (coreemail.Provider, coreemail.ResolvedFrom, error) {
	if tenantID != nil {
		t := *tenantID
		r.gotTenant = &t
	}
	return r.provider, r.from, nil
}

func TestServiceUsesResolverFromOverride(t *testing.T) {
	cap := &captureProvider{}
	resolver := &staticResolver{provider: cap, from: coreemail.ResolvedFrom{From: "tenant@example.com", ReplyTo: "support@example.com"}}
	svc, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@operator.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	orgID := "org-abc"
	if err := svc.SendOrgMemberInvite(context.Background(), orgID, "to@example.com", "Acme", "Alice", "tok", "key-1"); err != nil {
		t.Fatalf("SendOrgMemberInvite: %v", err)
	}
	if cap.got.From != "tenant@example.com" {
		t.Fatalf("expected From override, got %q", cap.got.From)
	}
	if cap.got.ReplyTo != "support@example.com" {
		t.Fatalf("expected ReplyTo override, got %q", cap.got.ReplyTo)
	}
	if resolver.gotTenant == nil || *resolver.gotTenant != orgID {
		t.Fatalf("expected resolver called with org %q, got %v", orgID, resolver.gotTenant)
	}
}

func TestServicePlatformEmailPassesNilTenant(t *testing.T) {
	cap := &captureProvider{}
	resolver := &staticResolver{provider: cap, from: coreemail.ResolvedFrom{}}
	svc, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@operator.com",
		ReplyTo:          "reply@operator.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	if err := svc.SendMagicLink(context.Background(), "to@example.com", "tok", "key-2"); err != nil {
		t.Fatalf("SendMagicLink: %v", err)
	}
	if resolver.gotTenant != nil {
		t.Fatalf("expected nil tenant for platform email, got %q", *resolver.gotTenant)
	}
	if cap.got.From != "noreply@operator.com" {
		t.Fatalf("expected operator From, got %q", cap.got.From)
	}
	if cap.got.ReplyTo != "reply@operator.com" {
		t.Fatalf("expected operator ReplyTo, got %q", cap.got.ReplyTo)
	}
}

func TestServiceLegacyNewServiceUsesOperatorOnly(t *testing.T) {
	cap := &captureProvider{}
	svc, err := coreemail.NewService(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@operator.com",
	}, cap)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.SendOrgMemberInvite(context.Background(), "org-x", "to@example.com", "Acme", "", "tok", "key-3"); err != nil {
		t.Fatalf("SendOrgMemberInvite: %v", err)
	}
	if cap.got.From != "noreply@operator.com" {
		t.Fatalf("expected operator From via legacy wrapper, got %q", cap.got.From)
	}
	if !strings.Contains(cap.got.TextBody, "Acme") {
		t.Fatalf("expected org name in body, got %q", cap.got.TextBody)
	}
	if errors.Unwrap(coreemail.NewPermanentError(errors.New("x"))) == nil {
		t.Fatal("smoke: PermanentError unwrap broken")
	}
}

func TestSendMagicLink(t *testing.T) {
	cap := &captureProvider{}
	resolver := &staticResolver{provider: cap, from: coreemail.ResolvedFrom{}}
	svc, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@operator.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}
	if err := svc.SendMagicLink(context.Background(), "user@example.com", "tok123", "magic_link:tok123"); err != nil {
		t.Fatalf("SendMagicLink: %v", err)
	}
	if cap.got.To != "user@example.com" {
		t.Fatalf("To = %q", cap.got.To)
	}
	if !strings.Contains(cap.got.TextBody, "/magic-link?token=tok123") {
		t.Fatalf("TextBody missing magic link: %q", cap.got.TextBody)
	}
}

func TestServiceSendTestUsesOrgIDWhenProvided(t *testing.T) {
	cap := &captureProvider{}
	resolver := &staticResolver{provider: cap, from: coreemail.ResolvedFrom{From: "tenant@example.com"}}
	svc, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@operator.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	if err := svc.SendTest(context.Background(), "org-y", "qa@example.com", "test-key"); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if resolver.gotTenant == nil || *resolver.gotTenant != "org-y" {
		t.Fatalf("expected resolver called with org-y, got %v", resolver.gotTenant)
	}
	if cap.got.To != "qa@example.com" {
		t.Fatalf("To: got %q", cap.got.To)
	}
	if cap.got.From != "tenant@example.com" {
		t.Fatalf("From override should apply, got %q", cap.got.From)
	}
}
