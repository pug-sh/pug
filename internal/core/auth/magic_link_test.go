package auth_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
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

func TestRequestMagicLink_IssuesTokenForKnownAndUnknownEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-known", Email: "known@example.com", DisplayName: "", PictureUri: "", PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	pub := &stubPublisher{}
	svc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), pub)

	for _, email := range []string{"known@example.com", "stranger@example.com"} {
		if err := svc.RequestMagicLink(ctx, email); err != nil {
			t.Fatalf("RequestMagicLink(%s): %v", email, err)
		}
	}

	// Both the known and the unknown email get a magic-link email with a token.
	if len(pub.jobs) != 2 {
		t.Fatalf("expected 2 published magic-link jobs, got %d", len(pub.jobs))
	}
	for _, pj := range pub.jobs {
		ml, ok := pj.job.Payload.(*emailworkerv1.EmailJob_MagicLink)
		if !ok {
			t.Fatalf("published job is not a magic link: %T", pj.job.Payload)
		}
		if ml.MagicLink.GetToken() == "" {
			t.Fatal("magic link job missing token")
		}
	}
}
