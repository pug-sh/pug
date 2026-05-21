package auth_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
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

func TestCompleteMagicLink_NewEmailCreatesVerifiedAccountAndOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	read := dbread.New(db.PgRO)
	ctx := context.Background()
	pub := &stubPublisher{}
	svc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), pub)

	if err := svc.RequestMagicLink(ctx, "fresh@example.com"); err != nil {
		t.Fatalf("RequestMagicLink: %v", err)
	}
	raw := lastMagicToken(t, pub)

	jwtTok, err := svc.CompleteMagicLink(ctx, raw)
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if jwtTok == "" {
		t.Fatal("expected a session JWT")
	}
	cust, err := read.GetCustomerByEmail(ctx, "fresh@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	if !cust.EmailVerifiedAt.Valid {
		t.Fatal("expected email_verified_at set")
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, cust.ID)
	if err != nil || len(orgs) == 0 {
		t.Fatalf("expected a default org, got %d (err=%v)", len(orgs), err)
	}

	// Single-use: a second completion fails.
	if _, err := svc.CompleteMagicLink(ctx, raw); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("second use err = %v, want ErrInvalidToken", err)
	}
}

func TestCompleteMagicLink_InvalidToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	svc := coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{})
	if _, err := svc.CompleteMagicLink(ctx, "no-such-token"); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func lastMagicToken(t *testing.T, pub *stubPublisher) string {
	t.Helper()
	if len(pub.jobs) == 0 {
		t.Fatal("no published jobs")
	}
	last := pub.jobs[len(pub.jobs)-1].job
	ml, ok := last.Payload.(*emailworkerv1.EmailJob_MagicLink)
	if !ok {
		t.Fatalf("last job is not a magic link: %T", last.Payload)
	}
	return ml.MagicLink.GetToken()
}
