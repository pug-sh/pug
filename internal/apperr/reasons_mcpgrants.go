package apperr

// MCP grants domain reasons.
var (
	ReasonMCPOAuthDisabled        = codes.add("MCP_OAUTH_DISABLED")
	ReasonMCPClientInvalid        = codes.add("MCP_CLIENT_INVALID")
	ReasonMCPClientUnavailable    = codes.add("MCP_CLIENT_UNAVAILABLE")
	ReasonMCPRedirectNotListed    = codes.add("MCP_REDIRECT_NOT_LISTED")
	ReasonMCPAuthorizationInvalid = codes.add("MCP_AUTHORIZATION_INVALID")
	ReasonMCPGrantNotFound        = codes.add("MCP_GRANT_NOT_FOUND")
	ReasonMCPNoOrg                = codes.add("MCP_NO_ORG")
)
