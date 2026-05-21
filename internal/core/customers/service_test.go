package customers_test

import (
	"context"
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

type stubPublisher struct{}

func (stubPublisher) Publish(context.Context, string, []byte) error { return nil }
