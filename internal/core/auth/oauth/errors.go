package oauth

import "errors"

var (
	ErrOAuthProviderDisabled    = errors.New("oauth provider disabled")
	ErrInvalidCredential        = errors.New("invalid oauth credential")
	ErrUnverifiedEmail          = errors.New("email not verified by identity provider")
	ErrIdentityResolutionFailed = errors.New("oauth identity resolution failed")
)

// ProviderName identifies an external identity provider stored in customer_identities.
type ProviderName string

const (
	ProviderGoogle ProviderName = "google"
)
