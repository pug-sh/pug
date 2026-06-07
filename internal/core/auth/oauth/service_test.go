package oauth_test

import (
	"context"
	"errors"
	"testing"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
)

type stubProvider struct {
	name coreoauth.ProviderName
}

func (s stubProvider) Name() coreoauth.ProviderName { return s.name }
func (s stubProvider) VerifyCredential(context.Context, string) (*coreoauth.Identity, error) {
	return nil, coreoauth.ErrInvalidCredential
}

func TestVerifyIdentity_RejectsWhenProviderDisabled(t *testing.T) {
	cfg := coreoauth.TestConfig("")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "credential")
	if !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

func TestVerifyIdentity_PropagatesInvalidCredential(t *testing.T) {
	cfg := coreoauth.TestConfig("client-id")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}))

	_, err := svc.VerifyIdentity(context.Background(), coreoauth.ProviderGoogle, "bad")
	if !errors.Is(err, coreoauth.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}
