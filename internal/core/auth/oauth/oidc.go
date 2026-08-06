package oauth

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
)

type oidcProvider struct {
	name     ProviderName
	verifier *oidc.IDTokenVerifier
}

func newOIDCProvider(ctx context.Context, cfg ProviderConfig) (*oidcProvider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oauth: discover oidc provider %q: %w", cfg.ID, err)
	}
	return &oidcProvider{
		name:     ProviderName(cfg.ID),
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (p *oidcProvider) Name() ProviderName { return p.name }

func (p *oidcProvider) VerifyCredential(ctx context.Context, credential string) (*Identity, error) {
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
