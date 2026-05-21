package auth

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// The auth handlers translate service sentinels into specific Connect codes.
// These pin the user-visible mappings (nil publisher is safe here: neither error
// path reaches an email publish).

func TestCompleteMagicLinkHandler_InvalidTokenMapsToInvalidArgument(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	srv := NewServer(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), nil)

	_, err := srv.CompleteMagicLink(context.Background(), connect.NewRequest(&authv1.CompleteMagicLinkRequest{
		Token: proto.String("nope-not-a-real-token"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument (err=%v)", got, err)
	}
}

func TestSignInWithEmailHandler_InvalidCredentialsMapsToUnauthenticated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	srv := NewServer(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), nil)

	_, err := srv.SignInWithEmail(context.Background(), connect.NewRequest(&authv1.SignInWithEmailRequest{
		Email:    proto.String("nobody@example.com"),
		Password: proto.String("whatever"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated (err=%v)", got, err)
	}
}
