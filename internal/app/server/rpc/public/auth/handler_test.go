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

// fakeAuthService satisfies authService so the handler's error mapping can be
// unit-tested without a database. Only the methods under test return errors.
type fakeAuthService struct {
	signInErr   error
	completeErr error
}

func (f fakeAuthService) SignInWithEmail(context.Context, string, string) (string, error) {
	return "", f.signInErr
}
func (f fakeAuthService) RequestMagicLink(context.Context, string) error { return nil }
func (f fakeAuthService) CompleteMagicLink(context.Context, string, string) (string, error) {
	return "", f.completeErr
}

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

// TestCompleteMagicLinkInvalidTokenMapping pins that an invalid/expired magic-link
// token maps to apperr Invalid / ReasonInvalidToken (CodeInvalidArgument), not a
// CodeInternal.
func TestCompleteMagicLinkInvalidTokenMapping(t *testing.T) {
	s := &server{service: fakeAuthService{completeErr: coreauth.ErrInvalidToken}}

	_, err := s.CompleteMagicLink(context.Background(), connect.NewRequest(&authv1.CompleteMagicLinkRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperr.Error, got %T", err)
	}
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidToken {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvalidToken)
	}
}
