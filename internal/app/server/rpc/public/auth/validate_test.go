package auth_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

func TestSignInWithEmailRequest_EmailRequired(t *testing.T) {
	req := &authv1.SignInWithEmailRequest{
		// email intentionally omitted
		Password: proto.String("password123"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing email, got nil")
	}
}

func TestSignUpWithEmailRequest_EmailRequired(t *testing.T) {
	req := &authv1.SignUpWithEmailRequest{
		// email intentionally omitted
		Password: proto.String("password123"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing email, got nil")
	}
}

func TestSignUpWithEmailRequest_EmailOptionalWithInviteToken(t *testing.T) {
	// With an invite_token the email may be omitted — it is sourced from the
	// invitation server-side. This is the message-level CEL accept branch.
	req := &authv1.SignUpWithEmailRequest{
		Password:    proto.String("password123"),
		InviteToken: proto.String("some-invite-token"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid (email optional with invite_token), got: %v", err)
	}
}

func TestSignUpWithEmailRequest_MalformedEmailRejectedWithInviteToken(t *testing.T) {
	// The field-level email rule applies unconditionally: a non-empty email must
	// be syntactically valid even when invite_token is set and the value is
	// otherwise ignored server-side.
	req := &authv1.SignUpWithEmailRequest{
		Email:       proto.String("not-an-email"),
		Password:    proto.String("password123"),
		InviteToken: proto.String("some-invite-token"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for malformed email even with invite_token, got nil")
	}
}

func TestSignUpWithEmailRequest_ValidEmailNoToken(t *testing.T) {
	req := &authv1.SignUpWithEmailRequest{
		Email:    proto.String("test@example.com"),
		Password: proto.String("password123"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestSignInWithEmailRequest_Valid(t *testing.T) {
	req := &authv1.SignInWithEmailRequest{
		Email:    proto.String("test@example.com"),
		Password: proto.String("password123"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestVerifyEmailRequest_TokenRequired(t *testing.T) {
	req := &authv1.VerifyEmailRequest{}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing token, got nil")
	}
}

func TestRequestPasswordResetRequest_Valid(t *testing.T) {
	req := &authv1.RequestPasswordResetRequest{Email: proto.String("test@example.com")}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestResetPasswordRequest_TokenRequired(t *testing.T) {
	req := &authv1.ResetPasswordRequest{Password: proto.String("password123")}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing token, got nil")
	}
}

func TestResendVerificationEmailRequest_Valid(t *testing.T) {
	req := &authv1.ResendVerificationEmailRequest{Email: proto.String("test@example.com")}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}
