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
func (s stubProvider) ExchangeCode(context.Context, coreoauth.AuthorizationCode) (*coreoauth.Identity, error) {
	return s.identity, s.err
}

func TestExchangeCodeReturnsVerifiedIdentity(t *testing.T) {
	identity, err := coreoauth.NewVerifiedIdentity(testProvider, coreoauth.Claims{
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

// A buggy provider must not file an identity under another provider's namespace.
func TestExchangeCodeRejectsMismatchedIdentityProvider(t *testing.T) {
	identity, err := coreoauth.NewVerifiedIdentity("someone_else", coreoauth.Claims{
		Subject: "employee-123", Email: "employee@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := coreoauth.NewService(coreoauth.TestConfig("client-id"), coreoauth.NewRegistry(stubProvider{name: testProvider, identity: identity}))

	_, err = svc.ExchangeCode(context.Background(), testProvider, coreoauth.AuthorizationCode{Code: "code"})
	if !errors.Is(err, coreoauth.ErrIdentityResolutionFailed) {
		t.Fatalf("err = %v, want ErrIdentityResolutionFailed", err)
	}
}

func TestExchangeCodeRejectsDisabledProvider(t *testing.T) {
	svc := coreoauth.NewService(coreoauth.TestConfig(""), coreoauth.NewRegistry(stubProvider{name: testProvider}))

	_, err := svc.ExchangeCode(context.Background(), testProvider, coreoauth.AuthorizationCode{Code: "code"})
	if !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

func TestExchangeCodePropagatesClientInputErrors(t *testing.T) {
	for _, sentinel := range []error{coreoauth.ErrInvalidCredential, coreoauth.ErrUnverifiedEmail} {
		svc := coreoauth.NewService(coreoauth.TestConfig("client-id"), coreoauth.NewRegistry(stubProvider{name: testProvider, err: sentinel}))
		_, err := svc.ExchangeCode(context.Background(), testProvider, coreoauth.AuthorizationCode{Code: "code"})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want %v", err, sentinel)
		}
	}
}

func TestExchangeCodeConvertsUnexpectedError(t *testing.T) {
	boom := errors.New("token endpoint exploded: secret-internal-detail")
	svc := coreoauth.NewService(coreoauth.TestConfig("client-id"), coreoauth.NewRegistry(stubProvider{name: testProvider, err: boom}))

	_, err := svc.ExchangeCode(context.Background(), testProvider, coreoauth.AuthorizationCode{Code: "code"})
	// Not ErrInvalidCredential: our fault, so the user must not be told to re-auth.
	if !errors.Is(err, coreoauth.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("server-side failure reported as a bad credential: %v", err)
	}
	if strings.Contains(err.Error(), "secret-internal-detail") {
		t.Fatalf("provider internal error leaked to caller: %v", err)
	}
}
