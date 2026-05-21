package templates

// invitePreview builds the hidden preheader (inbox preview snippet) for the
// org-invite email. It mirrors the body's inviter/no-inviter branch so the
// snippet never renders the broken " invited you to join ..." fragment when no
// inviter name is supplied.
func invitePreview(b Brand, orgName, inviterName string) string {
	if inviterName == "" {
		return "You've been invited to join " + orgName + " on " + b.ProductName
	}
	return inviterName + " invited you to join " + orgName
}
