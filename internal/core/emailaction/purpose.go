// Package emailaction defines shared value types for the email_action_tokens
// table. That table backs both passwordless login links and org-invite links;
// Purpose discriminates them.
package emailaction

// Purpose identifies what an email_action_tokens row authorizes. It is stored
// verbatim in the email_action_tokens.purpose column (varchar(30)) and gates how
// CompleteMagicLink redeems a token.
//
// Purposes are isolated by design: issuing or superseding a token of one purpose
// invalidates active tokens by (email, purpose), so it never touches a token of
// another purpose. That isolation is what keeps a plain login-link request from
// consuming a pending invite token. Defined once here so the auth (login) and
// orgs (invite) packages cannot drift on the stored values.
type Purpose string

const (
	// PurposeMagicLink is a passwordless login link (auth.RequestMagicLink).
	PurposeMagicLink Purpose = "magic_link"
	// PurposeOrgInvite is an organization invitation link (orgs invite issuance).
	PurposeOrgInvite Purpose = "org_invite"
)

// String returns the value as stored in the email_action_tokens.purpose column.
func (p Purpose) String() string { return string(p) }
