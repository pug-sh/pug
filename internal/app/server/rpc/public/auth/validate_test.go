package auth_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/public/auth/v1"
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
