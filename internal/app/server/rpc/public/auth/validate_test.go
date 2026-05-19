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
