package oauth

import "errors"

var (
	ErrOAuthProviderDisabled = errors.New("oauth provider disabled")
	ErrInvalidCredential     = errors.New("invalid oauth credential")
	ErrUnverifiedEmail       = errors.New("email not verified by identity provider")
	ErrNonASCIIEmail         = errors.New("email has non-ASCII characters")
	// The email, or the account it resolves to, is not on a domain the connection lists.
	ErrEmailNotOnConnection     = errors.New("email is not on a domain the sso connection lists")
	ErrIdentityResolutionFailed = errors.New("oauth identity resolution failed")
	// Our fault or the IdP's, so it must not reach the caller as a bad credential.
	ErrProviderUnavailable = errors.New("oauth provider unavailable")
)

// ProviderName is a configured provider's id ("google", "okta", "company_sso").
// It is both the registry key and the value stored in customer_identities.provider,
// so it is a permanent identity key rather than a renameable label.
type ProviderName string
