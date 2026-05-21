package email

import (
	"fmt"
	"strings"
)

// magicLinkText is the plaintext twin of the magic-link email. Keep the raw
// URL on its own line so clients linkify it.
func magicLinkText(b Brand, link string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Sign in to %s\n\n", b.ProductName)
	sb.WriteString("Use the link below to sign in. It works once and expires shortly.\n\n")
	sb.WriteString(link)
	sb.WriteString("\n\n")
	sb.WriteString("If you didn't request this, you can ignore this email.\n")
	return sb.String()
}

// inviteText is the plaintext twin of the org-invite email. Must contain the
// org name and the raw link.
func inviteText(b Brand, orgName, inviterName, link string) string {
	var sb strings.Builder
	if inviterName != "" {
		fmt.Fprintf(&sb, "%s invited you to join %s on %s.\n\n", inviterName, orgName, b.ProductName)
	} else {
		fmt.Fprintf(&sb, "You've been invited to join %s on %s.\n\n", orgName, b.ProductName)
	}
	sb.WriteString("Accept the invitation using the link below:\n\n")
	sb.WriteString(link)
	sb.WriteString("\n\n")
	sb.WriteString("This invitation expires in 7 days.\n")
	return sb.String()
}

// providerTestText is the plaintext twin of the provider-test email.
func providerTestText(b Brand) string {
	return fmt.Sprintf("Your email provider is configured correctly.\n\n"+
		"This is a test message from %s. If you're reading it, your email provider "+
		"is configured correctly and able to deliver mail.\n", b.ProductName)
}
