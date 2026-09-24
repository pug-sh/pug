package apperr

// Projects domain reasons.
var (
	ReasonProjectNotFound              = codes.add("PROJECT_NOT_FOUND")
	ReasonInvalidTimezone              = codes.add("INVALID_TIMEZONE")
	ReasonApiKeyNotFound               = codes.add("API_KEY_NOT_FOUND")
	ReasonDeletionConfirmationMismatch = codes.add("DELETION_CONFIRMATION_MISMATCH")
	ReasonDeletionBlocked              = codes.add("DELETION_BLOCKED")
)
