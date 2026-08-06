package oauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
)

type stubProvider struct {
	name     coreoauth.ProviderName
	identity *coreoauth.Identity
	err      error
}

const testProvider = coreoauth.ProviderName("test_oidc")

func (s stubProvider) Name() coreoauth.ProviderName { return s.name }
func (s stubProvider) VerifyCredential(context.Context, string) (*coreoauth.Identity, error) {
	return s.identity, s.err
}
func (s stubProvider) ExchangeCode(context.Context, coreoauth.AuthorizationCode) (*coreoauth.Identity, error) {
	return s.identity, s.err
}

func TestVerifyIdentityReturnsVerifiedIdentity(t *testing.T) {
	identity, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{
		Subject: "employee-123", Email: "employee@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: testProvider, identity: identity}))

	got, err := svc.VerifyIdentity(context.Background(), testProvider, "credential")
	if err != nil {
		t.Fatal(err)
	}
	if got != identity || got.Provider() != testProvider {
		t.Fatalf("identity = %+v, provider = %q", got, got.Provider())
	}
}

func TestVerifyIdentity_RejectsWhenProviderDisabled(t *testing.T) {
	cfg := coreoauth.TestConfig("")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: testProvider, err: coreoauth.ErrInvalidCredential}))

	_, err := svc.VerifyIdentity(context.Background(), testProvider, "credential")
	if !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

func TestVerifyIdentity_PropagatesInvalidCredential(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: testProvider, err: coreoauth.ErrInvalidCredential}))

	_, err := svc.VerifyIdentity(context.Background(), testProvider, "bad")
	if !errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

// TestVerifyIdentity_PropagatesUnverifiedEmail pins that an unverified-email
// rejection from the provider is NOT collapsed into ErrInvalidCredential — the
// handler maps the two to different reasons/codes, so the distinction matters.
func TestVerifyIdentity_PropagatesUnverifiedEmail(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: testProvider, err: coreoauth.ErrUnverifiedEmail}))

	_, err := svc.VerifyIdentity(context.Background(), testProvider, "cred")
	if !errors.Is(err, coreoauth.ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
	}
}

// TestVerifyIdentity_ConvertsUnexpectedErrorToInvalidCredential pins that a
// non-sentinel verifier error is converted to ErrInvalidCredential AND that the
// internal error string does not leak to the caller.
func TestVerifyIdentity_ConvertsUnexpectedErrorToInvalidCredential(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	boom := errors.New("jwks endpoint exploded: secret-internal-detail")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: testProvider, err: boom}))

	_, err := svc.VerifyIdentity(context.Background(), testProvider, "cred")
	if !errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	if strings.Contains(err.Error(), "secret-internal-detail") {
		t.Fatalf("verifier internal error leaked to caller: %v", err)
	}
}

func TestExchangeCodeReturnsVerifiedIdentity(t *testing.T) {
	identity, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{
		Subject: "employee-123", Email: "employee@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := coreoauth.NewService(coreoauth.TestConfig("client-id"), coreoauth.NewRegistry(stubProvider{name: testProvider, identity: identity}))

	got, err := svc.ExchangeCode(context.Background(), testProvider, coreoauth.AuthorizationCode{Code: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if got != identity || got.Provider() != testProvider {
		t.Fatalf("identity = %+v, provider = %q", got, got.Provider())
	}
}
