package oauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
)

type stubProvider struct {
	name coreoauth.ProviderName
	err  error
}

func (s stubProvider) Name() coreoauth.ProviderName { return s.name }
func (s stubProvider) VerifyCredential(context.Context, string) (*coreoauth.Identity, error) {
	return nil, s.err
}

func TestVerifyIdentity_RejectsWhenProviderDisabled(t *testing.T) {
	cfg := coreoauth.TestConfig("")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle, err: coreoauth.ErrInvalidCredential}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "credential")
	if !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

func TestVerifyIdentity_PropagatesInvalidCredential(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle, err: coreoauth.ErrInvalidCredential}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "bad")
	if !errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

// TestVerifyIdentity_PropagatesUnverifiedEmail pins that an unverified-email
// rejection from the provider is NOT collapsed into ErrInvalidCredential — the
// handler maps the two to different reasons/codes, so the distinction matters.
func TestVerifyIdentity_PropagatesUnverifiedEmail(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle, err: coreoauth.ErrUnverifiedEmail}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "cred")
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
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle, err: boom}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "cred")
	if !errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	if strings.Contains(err.Error(), "secret-internal-detail") {
		t.Fatalf("verifier internal error leaked to caller: %v", err)
	}
}
