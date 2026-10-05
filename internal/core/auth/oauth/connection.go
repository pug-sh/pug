package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"code.dny.dev/ssrf"
	"github.com/coreos/go-oidc/v3/oidc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// connectionProviderPrefix keys a connection's identities. Config provider ids can't contain ':'.
const connectionProviderPrefix = "conn:"

// go-oidc reads discovery documents and key sets whole.
const maxConnectionResponseBytes = 1 << 20

// net/http's default allows 10 MiB of headers.
const maxConnectionHeaderBytes = 64 << 10

// Bounds the endpoint cache; keys go stale when an issuer or client id changes.
const maxCachedConnections = 1000

// Company providers often listen on 8443.
var connectionGuard = ssrf.New(ssrf.WithAnyPort())

type Connection struct {
	ID           string
	Label        string
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// The verified domains it signs in. It refuses every other email.
	Domains []string
}

func ConnectionProviderName(connectionID string) ProviderName {
	return ProviderName(connectionProviderPrefix + connectionID)
}

var DefaultScopes = []string{"openid", "profile", "email"}

func (c Connection) providerConfig() ProviderConfig {
	return ProviderConfig{
		ID:           string(ConnectionProviderName(c.ID)),
		Type:         ProviderTypeOIDC,
		DisplayName:  c.Label,
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		IssuerURL:    c.IssuerURL,
		Scopes:       DefaultScopes,
		EmailDomains: c.Domains,
	}
}

// NewConnectionHTTPClient is the client for org-configured issuers. Unless allowPrivate,
// it refuses private, loopback and metadata addresses at dial time and ignores proxies.
// Every response body is capped at 1 MiB, and its headers at 64 KiB.
func NewConnectionHTTPClient(allowPrivate bool) *http.Client {
	return &http.Client{
		Timeout:   idpHTTPTimeout,
		Transport: otelhttp.NewTransport(cappedTransport{base: connectionTransport(allowPrivate)}),
	}
}

func connectionTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: idpHTTPTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = maxConnectionHeaderBytes
	if !allowPrivate {
		dialer.Control = connectionGuard.Safe
		// Behind a proxy the guard would check the proxy's address.
		transport.Proxy = nil
	}
	transport.DialContext = dialer.DialContext
	return transport
}

type cappedTransport struct {
	base http.RoundTripper
}

func (t cappedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxConnectionResponseBytes)
	return resp, nil
}

// DiscoverIssuer fetches the issuer's discovery document, so a wrong issuer fails on save.
func DiscoverIssuer(ctx context.Context, client *http.Client, issuerURL string) error {
	if _, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuerURL); err != nil {
		return fmt.Errorf("oauth: discover issuer: %w", err)
	}
	return nil
}

type connectionKey struct {
	id, issuer, clientID string
}

// connectionEndpoints caches discovery only. The secret and domains come from each sign-in's row.
type connectionEndpoints struct {
	mu sync.Mutex
	m  map[connectionKey]*endpoints
}

func (c *connectionEndpoints) get(k connectionKey) *endpoints {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *connectionEndpoints) put(k connectionKey, e *endpoints) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCachedConnections {
		c.m = make(map[connectionKey]*endpoints)
	}
	c.m[k] = e
}
