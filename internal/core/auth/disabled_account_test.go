package auth_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	serverrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/core/instance"
	"github.com/pug-sh/pug/internal/core/instanceadmin"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

func TestSessionRevocationAndDisabledAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	pub := &stubPublisher{}
	authSvc, err := coreauth.NewServiceForTest(ctx, db.PgRO, db.PgW, []byte("jwt-secret"), pub)
	if err != nil {
		t.Fatal(err)
	}
	const email = "suspend@example.com"
	if err := authSvc.RequestMagicLink(ctx, email); err != nil {
		t.Fatal(err)
	}
	first, err := authSvc.CompleteMagicLink(ctx, lastMagicToken(t, pub), "")
	if err != nil {
		t.Fatal(err)
	}
	read := dbread.New(db.PgRO)
	user, err := read.GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	admin := instanceadmin.NewService(db.PgRO, db.PgW, instance.OpenPolicy(), nil)
	authenticate := func(token string) error {
		req, err := http.NewRequest(http.MethodGet, "/", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		_, err = serverrpc.WithJWTAuth([]byte("jwt-secret"), read)(ctx, req)
		return err
	}
	if err := authenticate(first.AccessToken); err != nil {
		t.Fatalf("initial JWT: %v", err)
	}
	if err := admin.RevokeUserSessions(ctx, user.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := authenticate(first.AccessToken); err == nil {
		t.Fatal("revoked JWT still works")
	}
	if _, err := authSvc.RefreshSession(ctx, first.RefreshToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("revoked refresh token: %v", err)
	}
	if err := authSvc.RequestMagicLink(ctx, email); err != nil {
		t.Fatal(err)
	}
	second, err := authSvc.CompleteMagicLink(ctx, lastMagicToken(t, pub), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticate(second.AccessToken); err != nil {
		t.Fatalf("new JWT after revoke: %v", err)
	}
	if err := admin.SetUserDisabled(ctx, user.ID, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := authenticate(second.AccessToken); err == nil {
		t.Fatal("disabled customer JWT still works")
	}
	if _, err := authSvc.RefreshSession(ctx, second.RefreshToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("disabled refresh token: %v", err)
	}
	if err := authSvc.RequestMagicLink(ctx, email); err != nil {
		t.Fatal(err)
	}
	if _, err := authSvc.CompleteMagicLink(ctx, lastMagicToken(t, pub), ""); !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("disabled magic link: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `update customers set password_hash=$2 where id=$1`, user.ID, string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := authSvc.SignInWithEmail(ctx, email, "test-password"); !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("disabled password sign-in: %v", err)
	}
}
