package oauth

import (
	"context"
	"strings"
)

// Claims are the raw, untrusted fields extracted from an IdP credential, before
// the verified-email invariant is enforced. Providers populate this and pass it
// to NewVerifiedIdentity.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	PictureURI    string
}

// Identity is a verified IdP identity. It can only be constructed via
// NewVerifiedIdentity, which guarantees a present, provider-verified email — so
// the resolver and provisioning never need to re-check EmailVerified. The fields
// are unexported precisely so an unverified identity is unrepresentable.
type Identity struct {
	provider    ProviderName
	subject     string
	email       string
	displayName string
	pictureURI  string
}

// NewVerifiedProviderIdentity builds a verified identity with the durable
// provider namespace used in customer_identities. The configured connection ID
// is deliberately separate and may be renamed without changing account identity.
func NewVerifiedProviderIdentity(provider ProviderName, c Claims) (*Identity, error) {
	ident, err := NewVerifiedIdentity(c)
	if err != nil {
		return nil, err
	}
	ident.provider = provider
	return ident, nil
}

// Display-field caps match the storage columns (customers.display_name
// varchar(150), customers.picture_uri varchar(255)). Clamping here keeps an
// oversized IdP claim from turning new-account creation into a CodeInternal.
const (
	maxDisplayNameLen = 150
	maxPictureURILen  = 255
)

// NewVerifiedIdentity enforces the security-critical invariant: the IdP must
// report a non-empty, verified email. It returns ErrUnverifiedEmail otherwise,
// and clamps display fields to their storage widths.
func NewVerifiedIdentity(c Claims) (*Identity, error) {
	if !c.EmailVerified || strings.TrimSpace(c.Email) == "" {
		return nil, ErrUnverifiedEmail
	}
	return &Identity{
		subject:     c.Subject,
		email:       c.Email,
		displayName: truncateRunes(c.DisplayName, maxDisplayNameLen),
		pictureURI:  truncateRunes(c.PictureURI, maxPictureURILen),
	}, nil
}

func (i *Identity) Subject() string        { return i.subject }
func (i *Identity) Provider() ProviderName { return i.provider }
func (i *Identity) Email() string          { return i.email }
func (i *Identity) DisplayName() string    { return i.displayName }
func (i *Identity) PictureURI() string     { return i.pictureURI }

// truncateRunes clamps s to at most max runes. Postgres varchar(n) counts
// characters, so truncating by rune (not byte) avoids splitting a multi-byte
// character at the boundary.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// Provider identifies one configured external identity provider.
type Provider interface {
	Name() ProviderName
}

// AuthorizationCode contains the browser-generated values needed to complete
// an OIDC Authorization Code + PKCE flow. Provider secrets never cross this
// boundary to the browser.
type AuthorizationCode struct {
	Code         string
	CodeVerifier string
	RedirectURI  string
	Nonce        string
}

// AuthorizationCodeProvider is implemented by providers that can exchange an
// authorization code on the server.
type AuthorizationCodeProvider interface {
	Provider
	ExchangeCode(ctx context.Context, code AuthorizationCode) (*Identity, error)
}
