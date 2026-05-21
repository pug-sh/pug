package customers_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestSetPassword_ThenSignInSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-setpw", Email: "setpw@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	svc := corecustomers.NewService(db.PgW)
	if err := svc.SetPassword(ctx, "cust-setpw", "brand-new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	authSvc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), stubPublisher{})
	if _, err := authSvc.SignInWithEmail(ctx, "setpw@example.com", "brand-new-password"); err != nil {
		t.Fatalf("SignInWithEmail after SetPassword: %v", err)
	}
}

// SetPassword overwrites any existing hash: after re-setting, the old password
// no longer authenticates and the new one does.
func TestSetPassword_OverwritesExistingHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-setpw-ow", Email: "setpw-ow@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	svc := corecustomers.NewService(db.PgW)
	if err := svc.SetPassword(ctx, "cust-setpw-ow", "first-password"); err != nil {
		t.Fatalf("SetPassword first: %v", err)
	}
	if err := svc.SetPassword(ctx, "cust-setpw-ow", "second-password"); err != nil {
		t.Fatalf("SetPassword second: %v", err)
	}

	authSvc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), stubPublisher{})
	if _, err := authSvc.SignInWithEmail(ctx, "setpw-ow@example.com", "first-password"); !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("old password should be rejected after overwrite, got %v", err)
	}
	if _, err := authSvc.SignInWithEmail(ctx, "setpw-ow@example.com", "second-password"); err != nil {
		t.Fatalf("new password should authenticate, got %v", err)
	}
}

type stubPublisher struct{}

func (stubPublisher) Publish(context.Context, string, []byte) error { return nil }
