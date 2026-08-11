package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/pug-sh/pug/internal/httpx"
	"golang.org/x/oauth2"
)

// RFC 6749 §5.2: the token-endpoint error meaning the caller's code or verifier was bad.
const errCodeInvalidGrant = "invalid_grant"

// go-oidc retains this client for later JWKS fetches, so the bound lives on the
// client rather than a context deadline, which would expire and break them.
const idpHTTPTimeout = 10 * time.Second

// DefaultHTTPClient is the client used to reach configured IdPs. Callers may
// substitute their own; the timeout policy lives here because it is go-oidc's
// client-retention behaviour that makes it load-bearing.
func DefaultHTTPClient() *http.Client {
	return httpx.NewClient(idpHTTPTimeout)
}

type endpoints struct {
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Endpoint
}

type oidcProvider struct {
	name       ProviderName
	cfg        ProviderConfig
	httpClient *http.Client

	mu         sync.Mutex
	discovered *endpoints
}

func newOIDCProvider(cfg ProviderConfig, httpClient *http.Client) *oidcProvider {
	if httpClient == nil {
		httpClient = DefaultHTTPClient()
	}
	return &oidcProvider{
		name:       ProviderName(cfg.ID),
		cfg:        cfg,
		httpClient: httpClient,
	}
}

func (p *oidcProvider) Name() ProviderName { return p.name }

// Discovery runs on first use, not at boot, so an unreachable IdP can't stop the
// server starting. Failures aren't cached, so a provider recovers without a restart.
func (p *oidcProvider) discover(ctx context.Context) (*endpoints, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discovered != nil {
		return p.discovered, nil
	}
	// Callers queue here behind a slow issuer; one that gave up meanwhile must not
	// spend another timeout and log the resulting cancellation as a provider fault.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, p.httpClient), p.cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oauth: discover oidc provider %q: %w", p.name, err)
	}
	p.discovered = &endpoints{
		verifier: provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}),
		oauth:    provider.Endpoint(),
	}
	return p.discovered, nil
}

func (p *oidcProvider) ExchangeCode(ctx context.Context, input AuthorizationCode) (*Identity, error) {
	discovered, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	config := oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     discovered.oauth,
		Scopes:       p.cfg.Scopes,
		RedirectURL:  input.RedirectURI,
	}
	token, err := config.Exchange(oidc.ClientContext(ctx, p.httpClient), input.Code, oauth2.VerifierOption(input.CodeVerifier))
	if err != nil {
		// Only invalid_grant is the caller's fault; an IdP outage or a bad client
		// secret must not masquerade as a bad login.
		if retrieve, ok := errors.AsType[*oauth2.RetrieveError](err); ok && retrieve.ErrorCode == errCodeInvalidGrant {
			return nil, fmt.Errorf("%w: authorization code rejected", ErrInvalidCredential)
		}
		return nil, fmt.Errorf("oauth: code exchange with %q: %w", p.name, err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("oauth: %q returned no ID token", p.name)
	}
	return p.verifyIDToken(ctx, rawIDToken, input.Nonce)
}

func (p *oidcProvider) verifyIDToken(ctx context.Context, credential, expectedNonce string) (*Identity, error) {
	discovered, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	idToken, err := discovered.verifier.Verify(ctx, credential)
	if err != nil {
		// Expiry is a slow user; anything else points at our config or the IdP.
		if _, ok := errors.AsType[*oidc.TokenExpiredError](err); ok {
			return nil, fmt.Errorf("%w: ID token expired", ErrInvalidCredential)
		}
		return nil, fmt.Errorf("oauth: verify ID token from %q: %w", p.name, err)
	}
	// An absent expected nonce is our bug, not a bad login.
	if expectedNonce == "" {
		return nil, fmt.Errorf("oauth: %q: no expected nonce to verify against", p.name)
	}
	if idToken.Nonce != expectedNonce {
		// The browser picked this nonce and echoes it back, so a mismatch means the
		// ID token belongs to a different authorization request.
		slog.WarnContext(ctx, "oidc nonce mismatch", slog.String("provider", string(p.name)),
			slog.Bool("token_nonce_present", idToken.Nonce != ""))
		return nil, fmt.Errorf("%w: nonce mismatch", ErrInvalidCredential)
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oauth: decode claims from %q: %w", p.name, err)
	}

	// sub is unique only within one IdP, so the account is keyed (provider id, sub).
	// Renaming a provider id in config makes the next sign-in miss its old row.
	return NewVerifiedIdentity(p.name, Claims{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
		PictureURI:    claims.Picture,
	})
}
