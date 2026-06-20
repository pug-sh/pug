package apperr

// Profiles domain reasons.
var (
	ReasonProfileNotFound         = codes.add("PROFILE_NOT_FOUND")
	ReasonInvalidPageToken        = codes.add("INVALID_PAGE_TOKEN")
	ReasonInvalidProfileFilter    = codes.add("INVALID_PROFILE_FILTER")
	ReasonDeletionRequestNotFound = codes.add("DELETION_REQUEST_NOT_FOUND")
)
