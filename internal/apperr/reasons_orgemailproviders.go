package apperr

// OrgEmailProviders domain reasons.
var (
	ReasonEmailProviderNotFound          = codes.add("EMAIL_PROVIDER_NOT_FOUND")
	ReasonEmailProviderEncryptionMissing = codes.add("EMAIL_PROVIDER_ENCRYPTION_KEY_MISSING")
	ReasonEmailProviderDecryptFailed     = codes.add("EMAIL_PROVIDER_SECRET_DECRYPT_FAILED")
	ReasonInvalidEmailProviderConfig     = codes.add("INVALID_EMAIL_PROVIDER_CONFIG")
	ReasonEmailTestSendUnavailable       = codes.add("EMAIL_TEST_SEND_UNAVAILABLE")
	ReasonEmailTestRecipientMismatch     = codes.add("EMAIL_TEST_RECIPIENT_MISMATCH")
)
