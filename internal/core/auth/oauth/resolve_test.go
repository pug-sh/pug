package oauth_test

import (
	"context"
	"errors"
	"testing"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestResolveIdentity_LinksExistingEmailPasswordCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	write := dbwrite.New(db.PgW)
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-oauth-link", Email: "oauth-link@example.com", DisplayName: "Existing",
		PictureUri: "", PasswordHash: string(hash),
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	customerID, createdNew, err := coreoauth.ResolveIdentity(ctx, db.PgW, coreoauth.ProviderGoogle, &coreoauth.Identity{
		Subject: "google-sub-1", Email: "oauth-link@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if createdNew {
		t.Fatal("expected link to existing customer, not new account")
	}
	if customerID != "cust-oauth-link" {
		t.Fatalf("customer_id = %q, want cust-oauth-link", customerID)
	}

	read := dbread.New(db.PgRO)
	ident, err := read.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
		Provider: string(coreoauth.ProviderGoogle), ProviderSubject: "google-sub-1",
	})
	if err != nil {
		t.Fatalf("GetCustomerIdentityByProviderSubject: %v", err)
	}
	if ident.CustomerID != "cust-oauth-link" {
		t.Fatalf("identity customer_id = %q", ident.CustomerID)
	}
}

func TestResolveIdentity_CreatesNewCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	customerID, createdNew, err := coreoauth.ResolveIdentity(ctx, db.PgW, coreoauth.ProviderGoogle, &coreoauth.Identity{
		Subject: "google-sub-new", Email: "oauth-new@example.com", EmailVerified: true,
		DisplayName: "OAuth User", PictureURI: "https://example.com/pic.png",
	})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if !createdNew {
		t.Fatal("expected new account")
	}
	if customerID == "" {
		t.Fatal("expected customer id")
	}

	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByID(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCustomerByID: %v", err)
	}
	if customer.Email != "oauth-new@example.com" {
		t.Fatalf("email = %q", customer.Email)
	}
	if customer.DisplayName != "OAuth User" {
		t.Fatalf("display_name = %q", customer.DisplayName)
	}
}

func TestResolveIdentity_RejectsUnverifiedEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	_, _, err := coreoauth.ResolveIdentity(context.Background(), db.PgW, coreoauth.ProviderGoogle, &coreoauth.Identity{
		Subject: "sub", Email: "a@b.com", EmailVerified: false,
	})
	if !errors.Is(err, coreoauth.ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
	}
}

func TestWithIdentityTx_ConcurrentSignupSameEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	const email = "oauth-race@example.com"
	idents := []*coreoauth.Identity{
		{Subject: "google-sub-race-a", Email: email, EmailVerified: true, DisplayName: "Race A"},
		{Subject: "google-sub-race-b", Email: email, EmailVerified: true, DisplayName: "Race B"},
	}

	errCh := make(chan error, len(idents))
	for _, ident := range idents {
		go func(ident *coreoauth.Identity) {
			_, _, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
			errCh <- err
		}(ident)
	}
	for range idents {
		if err := <-errCh; err != nil {
			t.Fatalf("WithIdentityTx: %v", err)
		}
	}

	read := dbread.New(db.PgRO)
	customer, err := read.GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	for _, subject := range []string{"google-sub-race-a", "google-sub-race-b"} {
		ident, err := read.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
			Provider: string(coreoauth.ProviderGoogle), ProviderSubject: subject,
		})
		if err != nil {
			t.Fatalf("GetCustomerIdentityByProviderSubject(%q): %v", subject, err)
		}
		if ident.CustomerID != customer.ID {
			t.Fatalf("subject %q linked to %q, want %q", subject, ident.CustomerID, customer.ID)
		}
	}
}

func TestWithIdentityTx_RollsBackIdentityWhenFinalizeFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	ident := &coreoauth.Identity{
		Subject: "google-sub-rollback", Email: "oauth-rollback@example.com", EmailVerified: true,
	}
	var attempts int
	_, _, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, func(context.Context, *dbwrite.Queries, string, bool) error {
		attempts++
		return errors.New("simulated provisioning failure")
	})
	if err == nil {
		t.Fatal("expected finalize error")
	}

	read := dbread.New(db.PgRO)
	if _, err := read.GetCustomerByEmail(ctx, ident.Email); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected no customer after rollback, got err=%v", err)
	}

	_, createdNew, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
	if err != nil {
		t.Fatalf("retry WithIdentityTx: %v", err)
	}
	if !createdNew {
		t.Fatal("expected new account on retry after rollback")
	}
}
