package apperr

// Orgs domain reasons.
var (
	ReasonOrgNotFound               = codes.add("ORG_NOT_FOUND")
	ReasonOrgNotAMember             = codes.add("ORG_NOT_A_MEMBER")
	ReasonOrgAdminRequired          = codes.add("ORG_ADMIN_REQUIRED")
	ReasonOrgMemberNotFound         = codes.add("ORG_MEMBER_NOT_FOUND")
	ReasonOrgMemberAlreadyExists    = codes.add("ORG_MEMBER_ALREADY_EXISTS")
	ReasonOrgCannotRemoveSelf       = codes.add("ORG_CANNOT_REMOVE_SELF")
	ReasonOrgUnsupportedRole        = codes.add("ORG_UNSUPPORTED_ROLE")
	ReasonOrgUnsupportedRoleTransit = codes.add("ORG_UNSUPPORTED_ROLE_TRANSITION")
	ReasonCannotRemoveLastAdmin     = codes.add("CANNOT_REMOVE_LAST_ADMIN")
	ReasonCannotLeaveAsLastMember   = codes.add("CANNOT_LEAVE_AS_LAST_MEMBER")
	ReasonInvitationNotFound        = codes.add("INVITATION_NOT_FOUND")
	ReasonInvitationNotPending      = codes.add("INVITATION_NOT_PENDING")
	ReasonInvitationExpired         = codes.add("INVITATION_EXPIRED")
	ReasonInvitationAlreadyPending  = codes.add("INVITATION_ALREADY_PENDING")
	ReasonInvitationWrongEmail      = codes.add("INVITATION_WRONG_EMAIL")
	ReasonProjectNameTaken          = codes.add("PROJECT_NAME_TAKEN")
)
