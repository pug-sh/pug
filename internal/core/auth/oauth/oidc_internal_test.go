package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
	p := newOIDCProvider(ProviderConfig{ID: "company_sso", ClientID: clientID}, DefaultHTTPClient())
	p.discovered = &endpoints{
		verifier: oidc.NewVerifier(issuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{pub}}, &oidc.Config{ClientID: clientID}),
	}
	return p
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
	p.cfg.ClientSecret = "server-only-secret"
	p.discovered.oauth = oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}

	identity, err := p.ExchangeCode(context.Background(), AuthorizationCode{
		Code:         "authorization-code",
		CodeVerifier: codeVerifier,
		RedirectURI:  "https://pug.example.com/oauth/callback",
		Nonce:        nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email() != "employee@example.com" || identity.Provider() != "company_sso" {
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

const testNonce = "nonce-from-browser"

// Only a code the IdP blames on the caller may carry ErrInvalidCredential; if
// this classification broke, a provider outage would look like a bad login and
// an ordinary replayed code would spam ERROR logs.
func TestOIDCExchangeCodeClassifiesTokenEndpointFailures(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		body        string
		clientInput bool
	}{
		{"invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{"invalid_client", http.StatusUnauthorized, `{"error":"invalid_client"}`, false},
		{"provider outage", http.StatusServiceUnavailable, "upstream connect error", false},
		{"no id token", http.StatusOK, `{"access_token":"a","token_type":"Bearer"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			p := newOIDCProvider(ProviderConfig{ID: "company_sso", ClientID: testOIDCClientID}, DefaultHTTPClient())
			p.discovered = &endpoints{oauth: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}

			_, err := p.ExchangeCode(context.Background(), AuthorizationCode{Code: "code", Nonce: testNonce})
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrInvalidCredential) != tt.clientInput {
				t.Fatalf("err = %v, clientInput = %v", err, tt.clientInput)
			}
		})
	}
}

func validOIDCClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            issuer,
		"aud":            testOIDCClientID,
		"sub":            "employee-123",
		"nonce":          testNonce,
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

	t.Run("accepts a valid token and stores it under the provider id", func(t *testing.T) {
		identity, err := p.verifyIDToken(context.Background(), signToken(t, key, validOIDCClaims(issuer)), testNonce)
		if err != nil {
			t.Fatal(err)
		}
		if identity.Provider() != "company_sso" || identity.Subject() != "employee-123" || identity.Email() != "employee@example.com" {
			t.Fatalf("identity = provider %q, subject %q, email %q", identity.Provider(), identity.Subject(), identity.Email())
		}
	})

	// Fails closed, but as our bug rather than the caller's bad credential.
	t.Run("rejects an empty expected nonce", func(t *testing.T) {
		identity, err := p.verifyIDToken(context.Background(), signToken(t, key, validOIDCClaims(issuer)), "")
		if err == nil || identity != nil {
			t.Fatalf("identity = %v, err = %v, want rejection", identity, err)
		}
		if errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("err = %v, want an unclassified error", err)
		}
	})

	t.Run("rejects a token with no nonce claim", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		delete(claims, "nonce")
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), testNonce)
		if !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("rejects an expired token as client input", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), testNonce)
		if !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("rejects an unverified email", func(t *testing.T) {
		claims := validOIDCClaims(issuer)
		claims["email_verified"] = false
		_, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), testNonce)
		if !errors.Is(err, ErrUnverifiedEmail) {
			t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
		}
	})

	// Each check must reject on its own — a stray SkipIssuerCheck would pass every
	// other test here — and stay unclassified so Service logs it.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongAudience := validOIDCClaims(issuer)
	wrongAudience["aud"] = "another-client"
	wrongIssuer := validOIDCClaims(issuer)
	wrongIssuer["iss"] = "https://evil.example.com"
	malformedClaims := validOIDCClaims(issuer)
	malformedClaims["email"] = map[string]string{"unexpected": "object"}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"wrong audience", signToken(t, key, wrongAudience)},
		{"wrong issuer", signToken(t, key, wrongIssuer)},
		{"signed by an unknown key", signToken(t, otherKey, validOIDCClaims(issuer))},
		{"not a jwt", "definitely-not-a-jwt"},
		{"tampered signature", tamperSignature(t, signToken(t, key, validOIDCClaims(issuer)))},
		{"malformed claims", signToken(t, key, malformedClaims)},
	} {
		t.Run("rejects a token with "+tc.name, func(t *testing.T) {
			identity, err := p.verifyIDToken(context.Background(), tc.token, testNonce)
			if err == nil || identity != nil {
				t.Fatalf("identity = %v, err = %v, want rejection", identity, err)
			}
			if errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrUnverifiedEmail) {
				t.Fatalf("err = %v, want an unclassified error so the detect site logs it", err)
			}
		})
	}
}

// Mutates a decoded byte, not the last base64 char: that char holds only 2
// significant bits, so flipping it can re-decode identically and verify (~25% flake).
func tamperSignature(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-part JWT, got %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sig[0] ^= 0xFF
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	return strings.Join(parts, ".")
}

func discoveryServer(t *testing.T, failures int) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		hits++
		if hits <= failures {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`,
			server.URL, server.URL+"/authorize", server.URL+"/token", server.URL+"/keys")
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestOIDCProviderDiscoversLazilyAndCachesTheResult(t *testing.T) {
	server, hits := discoveryServer(t, 0)

	p := newOIDCProvider(ProviderConfig{ID: "company_sso", IssuerURL: server.URL, ClientID: testOIDCClientID}, DefaultHTTPClient())
	if *hits != 0 {
		t.Fatalf("construction performed discovery: %d hits", *hits)
	}
	if p.Name() != "company_sso" {
		t.Fatalf("name = %q", p.Name())
	}

	for range 2 {
		if _, err := p.discover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if *hits != 1 {
		t.Fatalf("hits = %d, want the discovery result cached after the first", *hits)
	}
}

// Pins that the injected client is the one actually used: go-oidc falls back to
// http.DefaultClient, so a broken hand-off would silently lose the timeout and
// the span rather than fail.
func TestOIDCProviderUsesTheInjectedHTTPClient(t *testing.T) {
	server, _ := discoveryServer(t, 0)
	var used int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used++
		return http.DefaultTransport.RoundTrip(r)
	})}

	p := newOIDCProvider(ProviderConfig{ID: "company_sso", IssuerURL: server.URL, ClientID: testOIDCClientID}, client)
	if _, err := p.discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("discovery bypassed the injected client")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A failure must not be cached, or a provider that was down at boot would stay
// broken until someone restarted the server.
func TestOIDCProviderRetriesFailedDiscovery(t *testing.T) {
	server, hits := discoveryServer(t, 1)
	p := newOIDCProvider(ProviderConfig{ID: "broken_sso", IssuerURL: server.URL, ClientID: "pug"}, DefaultHTTPClient())

	_, err := p.discover(context.Background())
	if err == nil || !containsAll(err.Error(), "discover oidc provider", "broken_sso") {
		t.Fatalf("err = %v", err)
	}

	if _, err := p.discover(context.Background()); err != nil {
		t.Fatalf("second discover: %v", err)
	}
	if *hits != 2 {
		t.Fatalf("hits = %d, want the failure retried", *hits)
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
