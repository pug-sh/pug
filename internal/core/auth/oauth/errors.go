package oauth

import "errors"

var (
	ErrOAuthProviderDisabled = errors.New("oauth provider disabled")
	ErrOAuthExchangeFailed   = errors.New("oauth exchange failed")
	ErrOAuthExchangeInvalid  = errors.New("oauth exchange invalid")
	ErrUnverifiedEmail       = errors.New("email not verified by identity provider")
	ErrInvalidState          = errors.New("invalid or expired oauth state")
	ErrInvalidRedirectURI    = errors.New("redirect uri not allowed")
)

// ProviderName identifies an external identity provider stored in customer_identities.
type ProviderName string

const (
	ProviderGoogle ProviderName = "google"
)
