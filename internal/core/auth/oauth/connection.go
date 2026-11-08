package oauth

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/pug-sh/pug/internal/httpx"
)

// connectionProviderPrefix keys a connection's identities. Config provider ids can't contain ':'.
const connectionProviderPrefix = "conn:"

// Bounds the endpoint cache; keys go stale when an issuer or client id changes.
const maxCachedConnections = 1000

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

// NewConnectionHTTPClient is the guarded client for org-configured issuers.
func NewConnectionHTTPClient(allowPrivate bool) *http.Client {
	return httpx.NewGuardedClient(idpHTTPTimeout, allowPrivate)
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
