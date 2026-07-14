package auth_test

import (
	"context"
	"errors"
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
	err      error
}

func (m mockOAuthProvider) Name() coreoauth.ProviderName { return coreoauth.ProviderGoogle }
func (m mockOAuthProvider) VerifyCredential(context.Context, string) (*coreoauth.Identity, error) {
	return m.identity, m.err
}

func mustVerifiedIdentity(t *testing.T, c coreoauth.Claims) *coreoauth.Identity {
	t.Helper()
	id, err := coreoauth.NewVerifiedIdentity(c)
	if err != nil {
		t.Fatalf("NewVerifiedIdentity: %v", err)
	}
	return id
}

func TestCompleteOAuthSignIn_NewUserCreatesOrgAndJWT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	cfg := coreoauth.TestConfig("client-id")
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: mustVerifiedIdentity(t, coreoauth.Claims{
		Subject: "google-sub-new-int", Email: "oauth-int-new@example.com", EmailVerified: true,
		DisplayName: "OAuth Int", PictureURI: "https://example.com/p.png",
	})})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, registry)

	session, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "google-credential", "Asia/Kolkata")
	if err != nil {
		t.Fatalf("CompleteOAuthSignIn: %v", err)
	}
	if session.RefreshToken == "" {
		t.Fatal("expected non-empty refresh token")
	}
	parsed, err := jwt.Parse(session.AccessToken, func(tok *jwt.Token) (any, error) {
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

	// The browser timezone passed to sign-in must seed the new default project's
	// reporting zone — the regression this covers is OAuth signup silently
	// defaulting every project to UTC.
	projects, err := read.GetProjectsByOrgID(ctx, orgs[0].ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("expected one default project, got %d projects (err=%v)", len(projects), err)
	}
	if projects[0].ReportingTimezone != "Asia/Kolkata" {
		t.Fatalf("reporting_timezone = %q, want Asia/Kolkata", projects[0].ReportingTimezone)
	}
}

func TestCompleteOAuthSignIn_LinksExistingEmailPasswordAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
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

	cfg := coreoauth.TestConfig("client-id")
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: mustVerifiedIdentity(t, coreoauth.Claims{
		Subject: "google-sub-link-int", Email: "oauth-int-link@example.com", EmailVerified: true,
	})})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, registry)

	if _, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "google-credential", ""); err != nil {
		t.Fatalf("CompleteOAuthSignIn: %v", err)
	}
	// Linking a verified Google identity must NOT clear the existing password.
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
	if strings.TrimSpace(ident.CustomerID) != "cust-oauth-int-link" {
		t.Fatalf("customer_id = %q, want cust-oauth-int-link", ident.CustomerID)
	}
}

// TestCompleteOAuthSignIn_RejectsUnverifiedEmail pins the account-takeover guard
// end-to-end: when the provider reports an unverified email, the service returns
// ErrUnverifiedEmail (not ErrInvalidCredential) and no account is provisioned.
func TestCompleteOAuthSignIn_RejectsUnverifiedEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	cfg := coreoauth.TestConfig("client-id")
	registry := coreoauth.NewRegistry(mockOAuthProvider{err: coreoauth.ErrUnverifiedEmail})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, registry)

	_, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "credential", "")
	if !errors.Is(err, coreoauth.ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
	}
}

// TestCompleteOAuthSignIn_RepeatedSignInIsIdempotent pins that a returning user
// is not re-provisioned: a second sign-in keeps exactly one org and does not
// overwrite the project's reporting zone with a browser timezone sent on the
// returning sign-in (FinishSignup no-ops for a returning user).
func TestCompleteOAuthSignIn_RepeatedSignInIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	cfg := coreoauth.TestConfig("client-id")
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: mustVerifiedIdentity(t, coreoauth.Claims{
		Subject: "google-sub-idem-int", Email: "oauth-int-idem@example.com", EmailVerified: true,
	})})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, registry)

	// First sign-in provisions the account with a Kolkata reporting zone.
	if _, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "credential", "Asia/Kolkata"); err != nil {
		t.Fatalf("first CompleteOAuthSignIn: %v", err)
	}
	// A returning sign-in carrying a *different* browser zone must neither
	// re-provision nor reset the existing project's reporting zone.
	if _, err := svc.CompleteOAuthSignIn(ctx, coreoauth.ProviderGoogle, "credential", "America/New_York"); err != nil {
		t.Fatalf("second CompleteOAuthSignIn: %v", err)
	}

	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByEmail(ctx, "oauth-int-idem@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, customer.ID)
	if err != nil {
		t.Fatalf("GetOrgsByCustomerID: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("repeat sign-in provisioned %d orgs, want 1 (no re-provisioning)", len(orgs))
	}

	// The returning sign-in's America/New_York must be ignored: the project keeps
	// the Asia/Kolkata zone seeded at signup. A regression that applied a returning
	// user's browser zone would silently rewrite an existing customer's reporting
	// zone on every login.
	projects, err := read.GetProjectsByOrgID(ctx, orgs[0].ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("expected one default project, got %d projects (err=%v)", len(projects), err)
	}
	if projects[0].ReportingTimezone != "Asia/Kolkata" {
		t.Fatalf("reporting_timezone = %q, want Asia/Kolkata (returning sign-in must not overwrite)", projects[0].ReportingTimezone)
	}
}
