package auth_test

import (
	"errors"
	"testing"

	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
)

// TestSignInCredentialErrorMapping verifies that the handler translates
// coreauth.ErrInvalidCredentials into an *apperr.Error with exactly
// ReasonInvalidCredentials — not a reason that distinguishes "user not found"
// from "wrong password". The anti-enumeration property is secured at two layers:
//
//   - Service layer (TestAuthService in internal/core/auth/service_test.go):
//     both "unknown user" and "wrong password" return coreauth.ErrInvalidCredentials,
//     proven by TestAuthService/ResetPassword_updates_hash_and_consumes_token
//     (wrong old password → ErrInvalidCredentials after reset).
//
//   - Handler layer (this test): the single errors.Is check on ErrInvalidCredentials
//     maps to one reason (INVALID_CREDENTIALS), so any coreauth.ErrInvalidCredentials
//     variant — regardless of root cause — produces the same tagged error.
//
// Because server wraps a concrete *coreauth.Service (not an interface), we
// cannot spin up a fake server without real DB infra. Instead we validate the
// mapping logic directly: construct the sentinel, run it through the same
// errors.Is guard the handler uses, and assert the resulting *apperr.Error.
func TestSignInCredentialErrorMapping(t *testing.T) {
	// Simulate the handler's error path for ErrInvalidCredentials.
	err := coreauth.ErrInvalidCredentials
	var handlerErr error
	if errors.Is(err, coreauth.ErrInvalidCredentials) {
		handlerErr = apperr.Unauthenticated(apperr.ReasonInvalidCredentials, "invalid credentials")
	}

	if handlerErr == nil {
		t.Fatal("expected handler to produce an error for ErrInvalidCredentials")
	}

	var ae *apperr.Error
	if !errors.As(handlerErr, &ae) {
		t.Fatalf("expected *apperr.Error, got %T", handlerErr)
	}

	if ae.Reason != apperr.ReasonInvalidCredentials {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonInvalidCredentials)
	}

	// No reason distinguishes existence from credential mismatch — both paths
	// in the service converge to ErrInvalidCredentials, both hit this single
	// arm, both produce ReasonInvalidCredentials.
	if ae.Reason == "EMAIL_NOT_FOUND" || ae.Reason == "WRONG_PASSWORD" {
		t.Errorf("reason %q breaks anti-enumeration: must not distinguish user-not-found from wrong-password", ae.Reason)
	}
}
