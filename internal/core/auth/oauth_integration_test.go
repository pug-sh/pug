package auth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

type mockOAuthProvider struct {
	identity *coreoauth.Identity
}

func (m mockOAuthProvider) Name() coreoauth.ProviderName { return coreoauth.ProviderGoogle }
func (m mockOAuthProvider) AuthorizationURL(state, redirectURI, codeChallenge string) string {
	return "https://accounts.example/o?state=" + state + "&challenge=" + codeChallenge + "&redirect_uri=" + redirectURI
}
func (m mockOAuthProvider) Exchange(context.Context, string, string, string) (*coreoauth.Identity, error) {
	return m.identity, nil
}

func TestCompleteOAuthSignIn_NewUserCreatesOrgAndJWT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	const redirectURI = "https://app.example/callback"
	cfg := coreoauth.TestConfig("client-id", "client-secret", redirectURI)
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: &coreoauth.Identity{
		Subject: "google-sub-new-int", Email: "oauth-int-new@example.com", EmailVerified: true,
		DisplayName: "OAuth Int", PictureURI: "https://example.com/p.png",
	}})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, rd.Client, cfg, registry)

	beginURL, state, err := svc.BeginOAuthSignIn(ctx, coreoauth.ProviderGoogle, redirectURI)
	if err != nil {
		t.Fatalf("BeginOAuthSignIn: %v", err)
	}
	if !strings.Contains(beginURL, "challenge=") {
		t.Fatalf("authorization_url missing PKCE challenge: %q", beginURL)
	}
	if state == "" {
		t.Fatal("expected non-empty state")
	}

	jwtTok, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "auth-code", state)
	if err != nil {
		t.Fatalf("CompleteOAuthSignIn: %v", err)
	}
	parsed, err := jwt.Parse(jwtTok, func(tok *jwt.Token) (any, error) {
		return []byte("test-secret-key-for-jwt"), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("JWT parse: %v", err)
	}
	sub, err := parsed.Claims.GetSubject()
	if err != nil || sub == "" {
		t.Fatalf("JWT subject: %v", err)
	}

	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByEmail(ctx, "oauth-int-new@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	if !customer.EmailVerifiedAt.Valid {
		t.Fatal("expected email_verified_at set")
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, customer.ID)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("expected one default org, got %d orgs (err=%v)", len(orgs), err)
	}
}

func TestCompleteOAuthSignIn_LinksExistingEmailPasswordAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-oauth-int-link", Email: "oauth-int-link@example.com", DisplayName: "Existing",
		PictureUri: "", PasswordHash: string(hash),
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	const redirectURI = "https://app.example/callback"
	cfg := coreoauth.TestConfig("client-id", "client-secret", redirectURI)
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: &coreoauth.Identity{
		Subject: "google-sub-link-int", Email: "oauth-int-link@example.com", EmailVerified: true,
	}})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, rd.Client, cfg, registry)

	_, state, err := svc.BeginOAuthSignIn(ctx, coreoauth.ProviderGoogle, redirectURI)
	if err != nil {
		t.Fatalf("BeginOAuthSignIn: %v", err)
	}
	if _, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "code", state); err != nil {
		t.Fatalf("CompleteOAuthSignIn: %v", err)
	}
	if _, err := svc.SignInWithEmail(ctx, "oauth-int-link@example.com", "password"); err != nil {
		t.Fatalf("password sign-in after google link: %v", err)
	}

	read := dbread.New(db.PgRO)
	ident, err := read.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
		Provider: string(coreoauth.ProviderGoogle), ProviderSubject: "google-sub-link-int",
	})
	if err != nil {
		t.Fatalf("GetCustomerIdentityByProviderSubject: %v", err)
	}
	if ident.CustomerID != "cust-oauth-int-link" {
		t.Fatalf("customer_id = %q, want cust-oauth-int-link", ident.CustomerID)
	}
}
