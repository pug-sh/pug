package oauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type googleProvider struct {
	clientID     string
	clientSecret string
	verifier     *oidc.IDTokenVerifier
}

const googleIssuer = "https://accounts.google.com"

func newGoogleProvider(ctx context.Context, clientID, clientSecret string) (*googleProvider, error) {
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("oauth: google oidc provider: %w", err)
	}
	return &googleProvider{
		clientID:     clientID,
		clientSecret: clientSecret,
		verifier:     provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (p *googleProvider) Name() ProviderName { return ProviderGoogle }

func (p *googleProvider) oauthConfig(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		RedirectURL:  redirectURI,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		Endpoint:     google.Endpoint,
	}
}

func (p *googleProvider) AuthorizationURL(state, redirectURI, codeChallenge string) string {
	return p.oauthConfig(redirectURI).AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

func (p *googleProvider) Exchange(ctx context.Context, redirectURI, code, codeVerifier string) (*Identity, error) {
	token, err := p.oauthConfig(redirectURI).Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", codeVerifier),
	)
	if err != nil {
		return nil, classifyExchangeError(err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("%w: missing id_token", ErrOAuthExchangeInvalid)
	}

	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("%w: id token: %v", ErrOAuthExchangeInvalid, err)
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrOAuthExchangeInvalid, err)
	}

	return &Identity{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
		PictureURI:    claims.Picture,
	}, nil
}

func classifyExchangeError(err error) error {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		switch retrieveErr.ErrorCode {
		case "invalid_grant", "invalid_request", "redirect_uri_mismatch", "unauthorized_client":
			return fmt.Errorf("%w: %v", ErrOAuthExchangeInvalid, err)
		}
	}
	return fmt.Errorf("%w: %v", ErrOAuthExchangeFailed, err)
}
