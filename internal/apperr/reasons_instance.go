package apperr

var (
	ReasonInstanceInvalidPageToken  = codes.add("INSTANCE_INVALID_PAGE_TOKEN")
	ReasonInstanceInvalidUserFilter = codes.add("INSTANCE_INVALID_USER_FILTER")
	ReasonInstanceAdminRequired     = codes.add("INSTANCE_ADMIN_REQUIRED")
	ReasonLastInstanceAdmin         = codes.add("LAST_INSTANCE_ADMIN")
	ReasonInstanceResourceNotFound  = codes.add("INSTANCE_RESOURCE_NOT_FOUND")
)
