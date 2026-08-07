package apperr

// SDK events domain reasons.
var (
	ReasonInvalidEventBatch             = codes.add("INVALID_EVENT_BATCH")
	ReasonCookielessIdentityUnavailable = codes.add("COOKIELESS_IDENTITY_UNAVAILABLE")
)
