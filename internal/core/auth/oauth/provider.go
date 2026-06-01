package oauth

import "context"

// Identity is the normalized result of a successful OAuth token exchange.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	PictureURI    string
}

// Provider implements authorization and token exchange for one IdP.
type Provider interface {
	Name() ProviderName
	AuthorizationURL(state, redirectURI, codeChallenge string) string
	Exchange(ctx context.Context, redirectURI, code, codeVerifier string) (*Identity, error)
}
