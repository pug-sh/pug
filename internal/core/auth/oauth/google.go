package oauth

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
)

type googleProvider struct {
	verifier *oidc.IDTokenVerifier
}

const googleIssuer = "https://accounts.google.com"

func newGoogleProvider(ctx context.Context, clientID string) (*googleProvider, error) {
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("oauth: google oidc provider: %w", err)
	}
	return &googleProvider{
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (p *googleProvider) Name() ProviderName { return ProviderGoogle }

func (p *googleProvider) VerifyCredential(ctx context.Context, credential string) (*Identity, error) {
	idToken, err := p.verifier.Verify(ctx, credential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
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

	// NewVerifiedIdentity enforces email_verified; an unverified Google email
	// surfaces as ErrUnverifiedEmail (distinct from ErrInvalidCredential).
	return NewVerifiedIdentity(Claims{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
		PictureURI:    claims.Picture,
	})
}
