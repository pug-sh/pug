package oauth

import "errors"

var (
	ErrOAuthProviderDisabled    = errors.New("oauth provider disabled")
	ErrInvalidCredential        = errors.New("invalid oauth credential")
	ErrUnverifiedEmail          = errors.New("email not verified by identity provider")
	ErrIdentityResolutionFailed = errors.New("oauth identity resolution failed")
	// Our fault or the IdP's, so it must not reach the caller as a bad credential.
	ErrProviderUnavailable = errors.New("oauth provider unavailable")
)

// ProviderName is a configured provider's id ("google", "okta", "company_sso").
// It is both the registry key and the value stored in customer_identities.provider,
// so it is a permanent identity key rather than a renameable label.
type ProviderName string
