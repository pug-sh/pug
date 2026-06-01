package oauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/testutil"
)

type stubProvider struct {
	name coreoauth.ProviderName
}

func (s stubProvider) Name() coreoauth.ProviderName { return s.name }
func (s stubProvider) AuthorizationURL(state, redirectURI, codeChallenge string) string {
	return "https://example.com/auth?state=" + state + "&challenge=" + codeChallenge + "&redirect=" + redirectURI
}
func (s stubProvider) Exchange(context.Context, string, string, string) (*coreoauth.Identity, error) {
	return nil, coreoauth.ErrOAuthExchangeFailed
}

func TestBegin_RejectsDisallowedRedirectURI(t *testing.T) {
	rd := testutil.SetupRedis(t)
	cfg := coreoauth.TestConfig("id", "secret", "https://app.example/callback")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}), coreoauth.NewStateStore(rd.Client))

	_, err := svc.Begin(context.Background(), coreoauth.ProviderGoogle, "https://evil.example/callback")
	if !errors.Is(err, coreoauth.ErrInvalidRedirectURI) {
		t.Fatalf("err = %v, want ErrInvalidRedirectURI", err)
	}
}

func TestComplete_RejectsMissingState(t *testing.T) {
	rd := testutil.SetupRedis(t)
	cfg := coreoauth.TestConfig("id", "secret")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}), coreoauth.NewStateStore(rd.Client))

	_, err := svc.ExchangeIdentity(context.Background(), coreoauth.ProviderGoogle, "code", "missing-state")
	if !errors.Is(err, coreoauth.ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func TestStateStore_GETDEL(t *testing.T) {
	rd := testutil.SetupRedis(t)
	store := coreoauth.NewStateStore(rd.Client)
	ctx := context.Background()

	if err := store.Save(ctx, coreoauth.ProviderGoogle, "https://app/cb", "verifier", "state123"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := store.Consume(ctx, "state123")
	if err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if st.RedirectURI != "https://app/cb" {
		t.Fatalf("redirect_uri = %q", st.RedirectURI)
	}
	if _, err = store.Consume(ctx, "state123"); !errors.Is(err, coreoauth.ErrInvalidState) {
		t.Fatalf("second Consume should return ErrInvalidState, got %v", err)
	}
}

func TestComplete_ProviderMismatchRejected(t *testing.T) {
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	const redirectURI = "https://app.example/callback"
	cfg := coreoauth.TestConfig("id", "secret", redirectURI)
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}), coreoauth.NewStateStore(rd.Client))

	// Inject state whose stored provider does not match the Complete request provider.
	const stateToken = "provider-mismatch"
	if err := rd.Client.Set(ctx, "oauth:state:"+stateToken, `{"provider":"github","code_verifier":"v","redirect_uri":"`+redirectURI+`"}`, 10*time.Minute).Err(); err != nil {
		t.Fatalf("redis set: %v", err)
	}

	_, err := svc.ExchangeIdentity(ctx, coreoauth.ProviderGoogle, "code", stateToken)
	if !errors.Is(err, coreoauth.ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func TestBegin_IncludesPKCEChallenge(t *testing.T) {
	rd := testutil.SetupRedis(t)
	cfg := coreoauth.TestConfig("id", "secret", "https://app.example/callback")
	svc := coreoauth.NewService(cfg, coreoauth.NewRegistry(stubProvider{name: coreoauth.ProviderGoogle}), coreoauth.NewStateStore(rd.Client))

	result, err := svc.Begin(context.Background(), coreoauth.ProviderGoogle, "https://app.example/callback")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !strings.Contains(result.AuthorizationURL, "challenge=") {
		t.Fatalf("authorization_url = %q, want PKCE challenge param", result.AuthorizationURL)
	}
}
