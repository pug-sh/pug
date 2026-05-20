package auth

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

// fakeAuthService satisfies authService; only SignInWithEmail is exercised here.
type fakeAuthService struct{ signInErr error }

func (f fakeAuthService) SignUpWithEmail(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (f fakeAuthService) SignInWithEmail(context.Context, string, string) (string, error) {
	return "", f.signInErr
}
func (f fakeAuthService) VerifyEmail(context.Context, string) error             { return nil }
func (f fakeAuthService) RequestPasswordReset(context.Context, string) error    { return nil }
func (f fakeAuthService) ResetPassword(context.Context, string, string) error   { return nil }
func (f fakeAuthService) ResendVerificationEmail(context.Context, string) error { return nil }

// TestSignInCredentialErrorMapping drives the real handler with a service that
// returns ErrInvalidCredentials and asserts it maps to the single, non-enumerating
// ReasonInvalidCredentials. Because it calls s.SignInWithEmail (not a copy of the
// mapping logic), a regression that distinguished "no such user" from "wrong
// password" would actually fail this test.
func TestSignInCredentialErrorMapping(t *testing.T) {
	s := &server{service: fakeAuthService{signInErr: coreauth.ErrInvalidCredentials}}

	_, err := s.SignInWithEmail(context.Background(), connect.NewRequest(&authv1.SignInWithEmailRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperr.Error, got %T", err)
	}
	if ae.Code() != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidCredentials {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvalidCredentials)
	}
	// Anti-enumeration: no reason may distinguish account existence from a bad password.
	if ae.Reason() == "EMAIL_NOT_FOUND" || ae.Reason() == "WRONG_PASSWORD" {
		t.Errorf("reason %q breaks anti-enumeration: must not distinguish user-not-found from wrong-password", ae.Reason())
	}
}
