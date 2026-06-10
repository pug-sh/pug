package oauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

func mustVerified(t *testing.T, c coreoauth.Claims) *coreoauth.Identity {
	t.Helper()
	id, err := coreoauth.NewVerifiedIdentity(c)
	if err != nil {
		t.Fatalf("NewVerifiedIdentity: %v", err)
	}
	return id
}

func TestWithIdentityTx_LinksExistingEmailPasswordCustomer(t *testing.T) {
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

	ident := mustVerified(t, coreoauth.Claims{Subject: "google-sub-1", Email: "oauth-link@example.com", EmailVerified: true})
	customerID, createdNew, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
	if err != nil {
		t.Fatalf("WithIdentityTx: %v", err)
	}
	if createdNew {
		t.Fatal("expected link to existing customer, not new account")
	}
	if strings.TrimSpace(customerID) != "cust-oauth-link" {
		t.Fatalf("customer_id = %q, want cust-oauth-link", customerID)
	}

	read := dbread.New(db.PgRO)
	identRow, err := read.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
		Provider: string(coreoauth.ProviderGoogle), ProviderSubject: "google-sub-1",
	})
	if err != nil {
		t.Fatalf("GetCustomerIdentityByProviderSubject: %v", err)
	}
	if strings.TrimSpace(identRow.CustomerID) != "cust-oauth-link" {
		t.Fatalf("identity customer_id = %q", identRow.CustomerID)
	}
}

func TestWithIdentityTx_CreatesNewCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	ident := mustVerified(t, coreoauth.Claims{
		Subject: "google-sub-new", Email: "oauth-new@example.com", EmailVerified: true,
		DisplayName: "OAuth User", PictureURI: "https://example.com/pic.png",
	})
	customerID, createdNew, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
	if err != nil {
		t.Fatalf("WithIdentityTx: %v", err)
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

// TestWithIdentityTx_IdempotentReSignIn pins the already-linked fast path: a
// repeat sign-in for the same (provider, subject) returns the same customer,
// reports createdNew=false, and does not create a duplicate account.
func TestWithIdentityTx_IdempotentReSignIn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	ident := mustVerified(t, coreoauth.Claims{
		Subject: "google-sub-idem", Email: "oauth-idem@example.com", EmailVerified: true,
	})

	id1, new1, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
	if err != nil {
		t.Fatalf("first WithIdentityTx: %v", err)
	}
	if !new1 {
		t.Fatal("first sign-in should create a new account")
	}

	id2, new2, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, nil)
	if err != nil {
		t.Fatalf("second WithIdentityTx: %v", err)
	}
	if new2 {
		t.Fatal("repeat sign-in must not create a second account")
	}
	if id1 != id2 {
		t.Fatalf("customer id changed across sign-ins: %q -> %q", id1, id2)
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
		mustVerified(t, coreoauth.Claims{Subject: "google-sub-race-a", Email: email, EmailVerified: true, DisplayName: "Race A"}),
		mustVerified(t, coreoauth.Claims{Subject: "google-sub-race-b", Email: email, EmailVerified: true, DisplayName: "Race B"}),
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
		identRow, err := read.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
			Provider: string(coreoauth.ProviderGoogle), ProviderSubject: subject,
		})
		if err != nil {
			t.Fatalf("GetCustomerIdentityByProviderSubject(%q): %v", subject, err)
		}
		if identRow.CustomerID != customer.ID {
			t.Fatalf("subject %q linked to %q, want %q", subject, identRow.CustomerID, customer.ID)
		}
	}
}

func TestWithIdentityTx_RollsBackIdentityWhenFinalizeFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	ident := mustVerified(t, coreoauth.Claims{
		Subject: "google-sub-rollback", Email: "oauth-rollback@example.com", EmailVerified: true,
	})
	var attempts int
	_, _, err := coreoauth.WithIdentityTx(ctx, db.PgW, coreoauth.ProviderGoogle, ident, func(context.Context, *dbwrite.Queries, string, bool) error {
		attempts++
		return errors.New("simulated provisioning failure")
	})
	if err == nil {
		t.Fatal("expected finalize error")
	}
	if attempts != 1 {
		t.Fatalf("finalize attempts = %d, want 1", attempts)
	}

	read := dbread.New(db.PgRO)
	if _, err := read.GetCustomerByEmail(ctx, ident.Email()); !errors.Is(err, pgx.ErrNoRows) {
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
