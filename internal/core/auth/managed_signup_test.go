package auth_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/instance"
	"github.com/pug-sh/pug/internal/core/instanceadmin"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

func managedPolicy(t *testing.T) instance.Policy {
	t.Helper()
	p, err := instance.ParsePolicy("managed", "operator@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManagedMagicLinkCreatesAccountWithoutOrganization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	pub := &stubPublisher{}
	svc, err := coreauth.NewServiceForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), pub, managedPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestMagicLink(ctx, "managed-magic@example.com"); err != nil {
		t.Fatal(err)
	}
	session, err := svc.CompleteMagicLink(ctx, lastMagicToken(t, pub), "")
	if err != nil || session.AccessToken == "" {
		t.Fatalf("CompleteMagicLink: session=%+v err=%v", session, err)
	}
	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByEmail(ctx, "managed-magic@example.com")
	if err != nil || !customer.EmailVerifiedAt.Valid {
		t.Fatalf("verified customer: %+v, %v", customer, err)
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, customer.ID)
	if err != nil || len(orgs) != 0 {
		t.Fatalf("organizations: %d, %v", len(orgs), err)
	}
}

func TestManagedOIDCCreatesAccountWithoutOrganization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	cfg := coreoauth.TestConfig("client-id")
	registry := coreoauth.NewRegistry(mockOAuthProvider{identity: mustVerifiedIdentity(t, coreoauth.Claims{
		Subject: "managed-oidc-sub", Email: "managed-oidc@example.com", EmailVerified: true,
	})})
	svc := coreauth.NewServiceWithOAuthForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, registry, managedPolicy(t))
	session, err := svc.CompleteOIDCSignIn(ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "authorization-code"}, "")
	if err != nil || session.AccessToken == "" {
		t.Fatalf("CompleteOIDCSignIn: session=%+v err=%v", session, err)
	}
	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByEmail(ctx, "managed-oidc@example.com")
	if err != nil || !customer.EmailVerifiedAt.Valid {
		t.Fatalf("verified customer: %+v, %v", customer, err)
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, customer.ID)
	if err != nil || len(orgs) != 0 {
		t.Fatalf("organizations: %d, %v", len(orgs), err)
	}
	admin := instanceadmin.NewService(db.PgRO, db.PgW, managedPolicy(t), nil)
	if err := admin.SetUserDisabled(ctx, customer.ID, customer.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteOIDCSignIn(ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "authorization-code"}, ""); !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("disabled OIDC sign-in: %v", err)
	}
}
