package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

const testGoogleClientID = "test-client-id.apps.googleusercontent.com"

// newTestVerifierProvider builds a googleProvider whose verifier trusts a static
// public key and the configured client ID — no network/OIDC discovery. This is
// the seam that lets us exercise the real VerifyCredential trust checks offline.
func newTestVerifierProvider(t *testing.T, pub crypto.PublicKey, clientID string) *googleProvider {
	t.Helper()
	ks := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{pub}}
	return &googleProvider{
		verifier: oidc.NewVerifier(googleIssuer, ks, &oidc.Config{ClientID: clientID}),
	}
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            googleIssuer,
		"aud":            testGoogleClientID,
		"sub":            "google-sub-123",
		"email":          "user@example.com",
		"email_verified": true,
		"name":           "Test User",
		"picture":        "https://example.com/p.png",
		"iat":            time.Now().Add(-time.Minute).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
}

func TestGoogleVerifyCredential_AcceptsValidToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	p := newTestVerifierProvider(t, &key.PublicKey, testGoogleClientID)

	id, err := p.VerifyCredential(context.Background(), signToken(t, key, validClaims()))
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if id.Subject() != "google-sub-123" || id.Email() != "user@example.com" {
		t.Fatalf("claims mismatch: sub=%q email=%q", id.Subject(), id.Email())
	}
}

// TestGoogleVerifyCredential_Rejects covers the trust boundary: a token that
// fails any of signature / audience / issuer / expiry must be rejected as
// ErrInvalidCredential. A regression that misconfigured the verifier (e.g. wrong
// ClientID, dropped issuer check) would fail here, not slip through.
func TestGoogleVerifyCredential_Rejects(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa (other): %v", err)
	}
	p := newTestVerifierProvider(t, &key.PublicKey, testGoogleClientID)

	wrongAud := validClaims()
	wrongAud["aud"] = "attacker-client-id.apps.googleusercontent.com"

	wrongIss := validClaims()
	wrongIss["iss"] = "https://evil.example.com"

	expired := validClaims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	cases := []struct {
		name  string
		token string
	}{
		{"wrong audience", signToken(t, key, wrongAud)},
		{"wrong issuer", signToken(t, key, wrongIss)},
		{"expired", signToken(t, key, expired)},
		{"signed by unknown key", signToken(t, otherKey, validClaims())},
		{"not a jwt", "definitely-not-a-jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.VerifyCredential(context.Background(), tc.token); !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("err = %v, want ErrInvalidCredential", err)
			}
		})
	}
}

func TestGoogleVerifyCredential_RejectsTamperedSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	p := newTestVerifierProvider(t, &key.PublicKey, testGoogleClientID)

	parts := strings.Split(signToken(t, key, validClaims()), ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-part JWT, got %d parts", len(parts))
	}

	// Tamper the signature bytes directly rather than flipping the token's
	// final character. A 256-byte RS256 signature base64url-encodes to a
	// length ≡ 2 (mod 4), so its last character holds only 2 significant bits
	// and Go's non-strict decoder discards the trailing padding bits — flipping
	// that char can decode to the identical signature and verify cleanly (a
	// ~25% flaky false negative). Mutating a decoded byte and re-encoding
	// alters the signature deterministically.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sig[0] ^= 0xFF
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)

	if _, err := p.VerifyCredential(context.Background(), strings.Join(parts, ".")); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

// TestGoogleVerifyCredential_RejectsUnverifiedEmail proves the real verifier
// path surfaces a false email_verified claim as ErrUnverifiedEmail (not as a
// successful sign-in and not as ErrInvalidCredential).
func TestGoogleVerifyCredential_RejectsUnverifiedEmail(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	p := newTestVerifierProvider(t, &key.PublicKey, testGoogleClientID)

	claims := validClaims()
	claims["email_verified"] = false
	if _, err := p.VerifyCredential(context.Background(), signToken(t, key, claims)); !errors.Is(err, ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
	}
}
