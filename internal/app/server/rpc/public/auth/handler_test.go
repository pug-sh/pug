package auth

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

// stubPublisher is a no-op coreauth.JobPublisher; the invite-error path under
// test returns before any email is published, so Publish is never invoked.
type stubPublisher struct{}

func (stubPublisher) Publish(context.Context, string, []byte) error { return nil }

// TestSignUpWithEmail_InviteInvalidMapsToFailedPrecondition pins the handler's
// translation of coreauth.ErrInviteInvalid → connect CodeFailedPrecondition.
// That code is the dashboard's signal to prompt for a fresh invite, so a
// regression to the CodeInternal fallthrough must fail the build. An unknown
// invite_token drives the service to ErrInviteInvalid before any customer is
// created; here we assert only the resulting connect code.
func TestSignUpWithEmail_InviteInvalidMapsToFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	srv := &server{service: coreauth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), stubPublisher{})}

	_, err := srv.SignUpWithEmail(context.Background(), connect.NewRequest(&authv1.SignUpWithEmailRequest{
		Password:    proto.String("password123"),
		InviteToken: proto.String("this-token-does-not-exist"),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
}
