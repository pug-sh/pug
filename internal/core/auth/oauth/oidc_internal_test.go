package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

const testOIDCClientID = "pug-dashboard"

func newTestOIDCVerifierProvider(pub crypto.PublicKey, issuer, clientID string) *oidcProvider {
	return &oidcProvider{
		name:     "company_sso",
		verifier: oidc.NewVerifier(issuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{pub}}, &oidc.Config{ClientID: clientID}),
	}
}

func TestOIDCExchangeCode(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	const (
		issuer       = "https://login.example.com/realms/main"
		nonce        = "nonce-from-browser"
		codeVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	)
	claims := validOIDCClaims(issuer)
	claims["nonce"] = nonce
	rawIDToken := signToken(t, key, claims)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("code") != "authorization-code" || r.Form.Get("code_verifier") != codeVerifier || r.Form.Get("redirect_uri") != "https://pug.example.com/oauth/callback" {
			t.Errorf("unexpected exchange form: %v", r.Form)
		}
		if r.Form.Get("client_id") != testOIDCClientID || r.Form.Get("client_secret") != "server-only-secret" {
			t.Errorf("client credentials were not sent to the provider: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"external-access-token","token_type":"Bearer","id_token":%q}`, rawIDToken)
	}))
	defer server.Close()

	p := newTestOIDCVerifierProvider(&key.PublicKey, issuer, testOIDCClientID)
	p.oauthConfig = oauth2.Config{
		ClientID:     testOIDCClientID,
		ClientSecret: "server-only-secret",
		Endpoint: oauth2.Endpoint{
			TokenURL:  server.URL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}

	identity, err := p.ExchangeCode(context.Background(), AuthorizationCode{
		Code:         "authorization-code",
		CodeVerifier: codeVerifier,
		RedirectURI:  "https://pug.example.com/oauth/callback",
		Nonce:        nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email() != "employee@example.com" || identity.Provider() != ProviderOIDC {
		t.Fatalf("identity = email %q, provider %q", identity.Email(), identity.Provider())
	}

	_, err = p.ExchangeCode(context.Background(), AuthorizationCode{
		Code:         "authorization-code",
		CodeVerifier: codeVerifier,
		RedirectURI:  "https://pug.example.com/oauth/callback",
		Nonce:        "wrong-nonce",
	})
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("nonce mismatch err = %v, want ErrInvalidCredential", err)
	}
}

func validOIDCClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            issuer,
		"aud":            testOIDCClientID,
		"sub":            "employee-123",
		"email":          "employee@example.com",
		"email_verified": true,
		"name":           "Example Employee",
		"picture":        "https://example.com/avatar.png",
		"iat":            time.Now().Add(-time.Minute).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func TestOIDCVerifyIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://login.example.com/realms/main"
	p := newTestOIDCVerifierProvider(&key.PublicKey, issuer, testOIDCClientID)

	t.Run("accepts a valid token and namespaces the subject by issuer", func(t *testing.T) {
		identity, err := p.verifyIDToken(context.Background(), signToken(t, key, validOIDCClaims(issuer)), "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != "company_sso" || identity.Provider() != ProviderOIDC {
			t.Fatalf("provider name = %q, identity namespace = %q", p.Name(), identity.Provider())
		}
		if identity.Subject() != issuer+"\x1femployee-123" || identity.Email() != "employee@example.com" {
			t.Fatalf("identity = subject %q, email %q", identity.Subject(), identity.Email())
		}
	})

	t.Run("rejects a token for another client", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		claims["aud"] = "another-client"
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), "")
		if !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("rejects malformed claims", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		claims["email"] = map[string]string{"unexpected": "object"}
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), "")
		if !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("rejects an unverified email", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		claims["email_verified"] = false
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), "")
		if !errors.Is(err, ErrUnverifiedEmail) {
			t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
		}
	})
}

func TestNewOIDCProviderDiscovery(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`,
			server.URL, server.URL+"/authorize", server.URL+"/token", server.URL+"/keys")
	}))
	defer server.Close()

	provider, err := newOIDCProvider(context.Background(), ProviderConfig{
		ID: "company_sso", IssuerURL: server.URL, ClientID: testOIDCClientID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "company_sso" {
		t.Fatalf("name = %q", provider.Name())
	}
}

func TestNewOIDCProviderReportsDiscoveryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := newOIDCProvider(context.Background(), ProviderConfig{ID: "broken_sso", IssuerURL: server.URL, ClientID: "pug"})
	if err == nil || !containsAll(err.Error(), "discover oidc provider", "broken_sso") {
		t.Fatalf("err = %v", err)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
