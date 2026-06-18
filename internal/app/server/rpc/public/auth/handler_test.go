package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
	"google.golang.org/protobuf/proto"
)

// fakeAuthService satisfies authService so the handler's error mapping can be
// unit-tested without a database. Only the methods under test return errors.
type fakeAuthService struct {
	signInErr        error
	completeErr      error
	completeOAuthErr error
	refreshErr       error
	revokeErr        error
}

func (f fakeAuthService) SignInWithEmail(context.Context, string, string) (coreauth.Session, error) {
	return coreauth.Session{}, f.signInErr
}
func (f fakeAuthService) RequestMagicLink(context.Context, string) error { return nil }
func (f fakeAuthService) CompleteMagicLink(context.Context, string, string) (coreauth.Session, error) {
	return coreauth.Session{}, f.completeErr
}
func (f fakeAuthService) CompleteOAuthSignIn(context.Context, coreoauth.ProviderName, string) (coreauth.Session, error) {
	return coreauth.Session{}, f.completeOAuthErr
}
func (f fakeAuthService) RefreshSession(context.Context, string) (coreauth.Session, error) {
	return coreauth.Session{}, f.refreshErr
}
func (f fakeAuthService) RevokeSession(context.Context, string) error {
	return f.revokeErr
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

func TestCompleteOAuthInvalidCredentialMapping(t *testing.T) {
	s := &server{service: fakeAuthService{completeOAuthErr: coreoauth.ErrInvalidCredential}}

	_, err := s.CompleteOAuthSignIn(context.Background(), connect.NewRequest(&authv1.CompleteOAuthSignInRequest{
		Provider:   authv1.OAuthProvider_O_AUTH_PROVIDER_GOOGLE.Enum(),
		Credential: proto.String("bad-credential"),
	}))
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
	if ae.Reason() != apperr.ReasonOAuthCredentialInvalid {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOAuthCredentialInvalid)
	}
}

func TestCompleteOAuthUnverifiedEmailMapping(t *testing.T) {
	s := &server{service: fakeAuthService{completeOAuthErr: coreoauth.ErrUnverifiedEmail}}

	_, err := s.CompleteOAuthSignIn(context.Background(), connect.NewRequest(&authv1.CompleteOAuthSignInRequest{
		Provider:   authv1.OAuthProvider_O_AUTH_PROVIDER_GOOGLE.Enum(),
		Credential: proto.String("credential"),
	}))
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
	if ae.Reason() != apperr.ReasonInvalidArgument {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvalidArgument)
	}
}

// TestRefreshSessionInvalidTokenMapping pins that an invalid/expired/revoked/reused
// refresh token maps to Unauthenticated / ReasonInvalidToken — deliberately
// DIFFERENT from CompleteMagicLink (which maps the same sentinel to InvalidArgument),
// because the FE's 401 handler is what clears the session on a failed refresh.
func TestRefreshSessionInvalidTokenMapping(t *testing.T) {
	s := &server{service: fakeAuthService{refreshErr: coreauth.ErrInvalidToken}}

	_, err := s.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{}))
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
	if ae.Reason() != apperr.ReasonInvalidToken {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvalidToken)
	}
}

// TestRefreshSessionInternalError pins that an unexpected (non-sentinel) error maps
// to a generic CodeInternal without leaking the underlying error string.
func TestRefreshSessionInternalError(t *testing.T) {
	s := &server{service: fakeAuthService{refreshErr: errors.New("db exploded with secret detail")}}

	_, err := s.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *connect.Error, got %T", err)
	}
	if ce.Code() != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", ce.Code())
	}
	if strings.Contains(ce.Message(), "secret detail") {
		t.Errorf("internal error leaked underlying message: %q", ce.Message())
	}
}

// TestSignOut pins that revoke failures surface as CodeInternal and success returns
// an empty response with no error.
func TestSignOut(t *testing.T) {
	t.Run("error maps to Internal", func(t *testing.T) {
		s := &server{service: fakeAuthService{revokeErr: errors.New("revoke failed")}}
		_, err := s.SignOut(context.Background(), connect.NewRequest(&authv1.SignOutRequest{}))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var ce *connect.Error
		if !errors.As(err, &ce) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if ce.Code() != connect.CodeInternal {
			t.Errorf("code = %v, want Internal", ce.Code())
		}
	})

	t.Run("success returns empty response", func(t *testing.T) {
		s := &server{service: fakeAuthService{}}
		resp, err := s.SignOut(context.Background(), connect.NewRequest(&authv1.SignOutRequest{}))
		if err != nil {
			t.Fatalf("SignOut: %v", err)
		}
		if resp == nil || resp.Msg == nil {
			t.Fatal("expected non-nil response")
		}
	})
}
