package customers_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
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

	authSvc, err := coreauth.NewServiceForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), stubPublisher{})
	if err != nil {
		t.Fatalf("NewServiceForTest: %v", err)
	}
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

	authSvc, err := coreauth.NewServiceForTest(ctx, db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), stubPublisher{})
	if err != nil {
		t.Fatalf("NewServiceForTest: %v", err)
	}
	if _, err := authSvc.SignInWithEmail(ctx, "setpw-ow@example.com", "first-password"); !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("old password should be rejected after overwrite, got %v", err)
	}
	if _, err := authSvc.SignInWithEmail(ctx, "setpw-ow@example.com", "second-password"); err != nil {
		t.Fatalf("new password should authenticate, got %v", err)
	}
}

type stubPublisher struct{}

func (stubPublisher) Publish(context.Context, string, []byte) error { return nil }

func TestSetPasswordRefusedWhereSSOIsRequired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()
	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)

	for _, c := range []dbwrite.CreateCustomerParams{
		{ID: "cust-admin", Email: "admin@acme.com"},
		{ID: "cust-bob", Email: "bob@acme.com"},
		{ID: "cust-carol", Email: "carol@globex.com"},
	} {
		if _, err := write.CreateCustomer(ctx, c); err != nil {
			t.Fatalf("CreateCustomer: %v", err)
		}
	}
	org, err := orgs.CreateOrgWithDefaults(ctx, "cust-admin", "acme")
	if err != nil {
		t.Fatal(err)
	}
	d, err := orgs.VerifyDomainByOperator(ctx, org.ID, "acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := coreorgs.MarkSSOSeenInTx(ctx, write, "acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := orgs.UpdateDomain(ctx, org.ID, d.ID, true); err != nil {
		t.Fatal(err)
	}

	svc := corecustomers.NewService(db.PgW)
	err = svc.SetPassword(ctx, "cust-bob", "brand-new-password")
	if ssoErr, ok := errors.AsType[*coreorgs.SSORequiredError](err); !ok || ssoErr.Domain != "acme.com" {
		t.Fatalf("err = %v, want SSORequiredError for acme.com", err)
	}
	if err := svc.SetPassword(ctx, "cust-carol", "brand-new-password"); err != nil {
		t.Fatalf("another domain: %v", err)
	}
}
