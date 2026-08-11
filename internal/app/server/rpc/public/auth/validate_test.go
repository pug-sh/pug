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

func TestSignInWithEmailRequest_Valid(t *testing.T) {
	req := &authv1.SignInWithEmailRequest{
		Email:    proto.String("test@example.com"),
		Password: proto.String("password123"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

const validCodeVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"

// The nonce and code_verifier length floors are the constraints that carry
// security weight: `required` is only a presence check under editions, so
// without min_len an empty nonce passes validation and reaches the verifier.
func TestCompleteOIDCSignInRequest_Validation(t *testing.T) {
	valid := func() *authv1.CompleteOIDCSignInRequest {
		return &authv1.CompleteOIDCSignInRequest{
			ProviderId:   proto.String("company_sso"),
			Code:         proto.String("authorization-code"),
			CodeVerifier: proto.String(validCodeVerifier),
			RedirectUri:  proto.String("https://pug.example.com/oauth/callback"),
			Nonce:        proto.String("nonce-of-16-chars"),
		}
	}

	if err := protovalidate.Validate(valid()); err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*authv1.CompleteOIDCSignInRequest)
	}{
		{"empty nonce", func(r *authv1.CompleteOIDCSignInRequest) { r.Nonce = proto.String("") }},
		{"short nonce", func(r *authv1.CompleteOIDCSignInRequest) { r.Nonce = proto.String("too-short") }},
		{"empty code verifier", func(r *authv1.CompleteOIDCSignInRequest) { r.CodeVerifier = proto.String("") }},
		{"short code verifier", func(r *authv1.CompleteOIDCSignInRequest) { r.CodeVerifier = proto.String(validCodeVerifier[:42]) }},
		{"code verifier with illegal char", func(r *authv1.CompleteOIDCSignInRequest) {
			r.CodeVerifier = proto.String(validCodeVerifier[:64] + "/=")
		}},
		{"empty code", func(r *authv1.CompleteOIDCSignInRequest) { r.Code = proto.String("") }},
		{"empty provider id", func(r *authv1.CompleteOIDCSignInRequest) { r.ProviderId = proto.String("") }},
		{"provider id with uppercase", func(r *authv1.CompleteOIDCSignInRequest) { r.ProviderId = proto.String("Company_SSO") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := valid()
			tt.mutate(req)
			if err := protovalidate.Validate(req); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}
