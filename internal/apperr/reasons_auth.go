package apperr

// Auth domain reasons. Credential/token flows use deliberately ambiguous reasons
// to avoid account-enumeration; see the design spec.
var (
	ReasonInvalidCredentials    = codes.add("INVALID_CREDENTIALS")
	ReasonInvalidToken          = codes.add("INVALID_TOKEN")
	ReasonPasswordTooLong       = codes.add("PASSWORD_TOO_LONG")
	ReasonOAuthProviderDisabled = codes.add("OAUTH_PROVIDER_DISABLED")
	ReasonOAuthExchangeInvalid  = codes.add("OAUTH_EXCHANGE_INVALID")
	ReasonOAuthExchangeFailed   = codes.add("OAUTH_EXCHANGE_FAILED")
)
