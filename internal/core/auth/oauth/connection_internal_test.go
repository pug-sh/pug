package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code.dny.dev/ssrf"
)

func TestConnectionSignsInOnlyListedDomains(t *testing.T) {
	const issuer = "https://acme.okta.com"
	for _, tc := range []struct {
		name     string
		email    string
		verified any
		wantErr  error
		proven   string
	}{
		{name: "listed domain needs no email_verified", email: "bob@acme.com", proven: "acme.com"},
		{name: "another domain is refused even when verified", email: "carol@globex.com", verified: true, wantErr: ErrEmailNotOnConnection},
		{name: "a personal address is refused", email: "jane@gmail.com", verified: true, wantErr: ErrEmailNotOnConnection},
		{name: "explicit false is still refused", email: "bob@acme.com", verified: false, wantErr: ErrUnverifiedEmail},
		{name: "no email claim is unverified, not another domain", email: "", wantErr: ErrUnverifiedEmail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, key := newTestProvider(t, ProviderConfig{ID: string(ConnectionProviderName("db0bnpqvh7le8fq2jqug")), IssuerURL: issuer, EmailDomains: []string{"acme.com"}}, issuer)
			claims := validOIDCClaims(issuer)
			claims["email"] = tc.email
			delete(claims, "email_verified")
			if tc.verified != nil {
				claims["email_verified"] = tc.verified
			}
			ident, err := p.verifyIDToken(context.Background(), signToken(t, key, claims), testNonce)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ident.ProvenDomain() != tc.proven {
				t.Fatalf("proven domain = %q, want %q", ident.ProvenDomain(), tc.proven)
			}
			if !ident.AllowsAccount("someone@acme.com") || ident.AllowsAccount("someone@globex.com") {
				t.Fatal("a connection identity must reach only accounts on its listed domains")
			}
		})
	}
}

func TestConfigProviderIdentityAllowsAnyAccount(t *testing.T) {
	const issuer = "https://login.example.com"
	p, key := newTestProvider(t, ProviderConfig{IssuerURL: issuer}, issuer)
	ident, err := p.verifyIDToken(context.Background(), signToken(t, key, validOIDCClaims(issuer)), testNonce)
	if err != nil {
		t.Fatal(err)
	}
	if !ident.AllowsAccount("anyone@globex.com") {
		t.Fatal("a config provider identity must keep reaching any account")
	}
}

func TestConnectionGuard(t *testing.T) {
	for _, tc := range []struct {
		network, addr string
		allowed       bool
	}{
		{"tcp4", "93.184.215.14:443", true},
		{"tcp4", "93.184.215.14:8443", true},
		{"tcp4", "127.0.0.1:443", false},
		{"tcp4", "10.1.2.3:443", false},
		{"tcp4", "192.168.1.1:443", false},
		{"tcp4", "169.254.169.254:80", false},
		{"tcp6", "[::1]:443", false},
		{"tcp6", "[fd00::1]:443", false},
	} {
		err := connectionGuard.Safe(tc.network, tc.addr, nil)
		if (err == nil) != tc.allowed {
			t.Errorf("Safe(%s) = %v, want allowed = %v", tc.addr, err, tc.allowed)
		}
	}
}

// Behind a proxy the dial guard would check the proxy's address, not the issuer's.
func TestConnectionTransportSkipsProxies(t *testing.T) {
	if connectionTransport(false).Proxy != nil {
		t.Fatal("the guarded transport must not use a proxy")
	}
	if connectionTransport(true).Proxy == nil {
		t.Fatal("with private issuers allowed, the transport keeps the environment's proxy")
	}
}

func TestConnectionClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guarded client must not reach a loopback server")
	}))
	defer srv.Close()

	if err := DiscoverIssuer(context.Background(), NewConnectionHTTPClient(false), srv.URL); !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Fatalf("err = %v, want ssrf.ErrProhibitedIP", err)
	}
}

// Whether an identity is a connection's comes from its conn: provider, so one built
// without the connection's domains reaches no account.
func TestConnectionIdentityWithoutDomainsAllowsNoAccount(t *testing.T) {
	ident, err := NewVerifiedIdentity(ConnectionProviderName("db0bnpqvh7le8fq2jqug"), Claims{Subject: "s", Email: "bob@acme.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if ident.AllowsAccount("bob@acme.com") {
		t.Fatal("a connection identity with no domains reached an account")
	}
}

// Library errors carry the IdP's response body, and a connection's IdP is an org admin's.
func TestIdentityErrorsAreLoggedTruncated(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	body := strings.Repeat("x", maxConnectionResponseBytes)
	s := NewService(Config{}, nil)
	for _, err := range []error{errors.New(body), fmt.Errorf("%w: %s", ErrUnverifiedEmail, body)} {
		logs.Reset()
		if _, got := s.handleIdentityResult(context.Background(), "conn:x", nil, err); got == nil {
			t.Fatal("want an error")
		}
		if logs.Len() > 4096 {
			t.Fatalf("logged %d bytes", logs.Len())
		}
	}
}

func TestConnectionClientCapsResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/headers" {
			w.Header().Set("X-Big", strings.Repeat("x", maxConnectionHeaderBytes))
		}
		_, _ = w.Write(make([]byte, maxConnectionResponseBytes+1))
	}))
	defer srv.Close()
	client := NewConnectionHTTPClient(true)

	resp, err := client.Get(srv.URL + "/body")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if _, ok := errors.AsType[*http.MaxBytesError](err); !ok {
		t.Fatalf("body: err = %v, want *http.MaxBytesError", err)
	}
	if resp, err := client.Get(srv.URL + "/headers"); err == nil {
		resp.Body.Close()
		t.Fatal("headers over the cap: want an error")
	}
}

// Without the guarded client a connection must not fall back to the default one.
func TestConnectionsOffWithoutAClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a service without a connection client reached the issuer")
	}))
	defer srv.Close()

	_, err := NewService(Config{}, nil).ExchangeConnectionCode(context.Background(), Connection{ID: "db0bnpqvh7le8fq2jqug", IssuerURL: srv.URL}, AuthorizationCode{})
	if !errors.Is(err, ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

// A client timeout wraps context.DeadlineExceeded while the caller still waits: an IdP outage.
func TestConnectionTimeoutIsProviderUnavailable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	s := NewService(Config{}, nil).WithConnections(&http.Client{Timeout: 50 * time.Millisecond})
	_, err := s.ExchangeConnectionCode(context.Background(), Connection{ID: "db0bnpqvh7le8fq2jqug", IssuerURL: srv.URL}, AuthorizationCode{})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
}

// A caller that gave up is not an IdP outage.
func TestCanceledSignInIsNotProviderUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewService(Config{}, nil).handleIdentityResult(ctx, "conn:x", nil, fmt.Errorf("exchange: %w", context.Canceled))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
