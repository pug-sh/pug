package oauth

import "context"

// Identity is the normalized result of a verified IdP credential.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	PictureURI    string
}

// Provider verifies IdP credentials for one identity provider.
type Provider interface {
	Name() ProviderName
	VerifyCredential(ctx context.Context, credential string) (*Identity, error)
}
