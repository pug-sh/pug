package auth_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestSignInWithEmail_EmptyPasswordHashIsInvalidCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	// A passwordless (magic-link) account: password_hash == "".
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-nopw", Email: "nopw@example.com", DisplayName: "", PictureUri: "", PasswordHash: "",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	svc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{})

	_, err := svc.SignInWithEmail(ctx, "nopw@example.com", "anything")
	if !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}
