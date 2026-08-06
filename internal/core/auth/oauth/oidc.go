package oauth

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type oidcProvider struct {
	name        ProviderName
	verifier    *oidc.IDTokenVerifier
	oauthConfig oauth2.Config
}

func newOIDCProvider(ctx context.Context, cfg ProviderConfig) (*oidcProvider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oauth: discover oidc provider %q: %w", cfg.ID, err)
	}
	return &oidcProvider{
		name:     ProviderName(cfg.ID),
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauthConfig: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			Scopes:       cfg.Scopes,
		},
	}, nil
}

func (p *oidcProvider) Name() ProviderName { return p.name }

func (p *oidcProvider) VerifyCredential(ctx context.Context, credential string) (*Identity, error) {
	return p.verifyIDToken(ctx, credential, "")
}

func (p *oidcProvider) ExchangeCode(ctx context.Context, input AuthorizationCode) (*Identity, error) {
	config := p.oauthConfig
	config.RedirectURL = input.RedirectURI
	token, err := config.Exchange(ctx, input.Code, oauth2.VerifierOption(input.CodeVerifier))
	if err != nil {
		return nil, fmt.Errorf("%w: authorization code exchange failed", ErrInvalidCredential)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("%w: token response did not contain an ID token", ErrInvalidCredential)
	}
	return p.verifyIDToken(ctx, rawIDToken, input.Nonce)
}

func (p *oidcProvider) verifyIDToken(ctx context.Context, credential, expectedNonce string) (*Identity, error) {
	idToken, err := p.verifier.Verify(ctx, credential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if expectedNonce != "" && idToken.Nonce != expectedNonce {
		return nil, fmt.Errorf("%w: nonce mismatch", ErrInvalidCredential)
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrInvalidCredential, err)
	}

	// OIDC only guarantees that sub is locally unique within one issuer. Prefixing
	// with the verified issuer produces a globally stable key while keeping the
	// configured connection ID free to change as an operator-facing label.
	return NewVerifiedProviderIdentity(ProviderOIDC, Claims{
		Subject:       idToken.Issuer + "\x1f" + idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
		PictureURI:    claims.Picture,
	})
}
