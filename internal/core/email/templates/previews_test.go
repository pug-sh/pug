package templates

import (
	"context"
	"strings"
	"testing"
)

func TestInvitePreviewWithInviter(t *testing.T) {
	got := invitePreview(Brand{ProductName: "Pug"}, "Acme", "Alice")
	if want := "Alice invited you to join Acme"; got != want {
		t.Fatalf("invitePreview = %q, want %q", got, want)
	}
}

func TestInvitePreviewWithoutInviter(t *testing.T) {
	got := invitePreview(Brand{ProductName: "Pug"}, "Acme", "")
	if want := "You've been invited to join Acme on Pug"; got != want {
		t.Fatalf("invitePreview = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, " ") {
		t.Fatalf("no-inviter preview must not start with a space: %q", got)
	}
}

// TestInviteRendersNoInviterPreheader confirms the Invite template actually
// wires invitePreview into the hidden preheader span, so the no-inviter path
// renders the corrected copy rather than the leading-space artifact.
func TestInviteRendersNoInviterPreheader(t *testing.T) {
	var buf strings.Builder
	err := Invite(Brand{ProductName: "Pug"}, "Acme", "", "https://app.example/x").Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := preheaderSpan(t, buf.String())
	if strings.HasPrefix(got, " ") {
		t.Fatalf("preheader starts with a space (no-inviter artifact): %q", got)
	}
	if !strings.Contains(got, "been invited to join Acme") {
		t.Fatalf("preheader missing no-inviter copy: %q", got)
	}
}

// preheaderSpan extracts the content of the hidden preview span emitted by the
// Layout template (raw templ output, before premailer).
func preheaderSpan(t *testing.T, html string) string {
	t.Helper()
	_, after, found := strings.Cut(html, `opacity:0;">`)
	if !found {
		t.Fatalf("preview span not found in rendered html")
	}
	content, _, found := strings.Cut(after, "</span>")
	if !found {
		t.Fatalf("preview span not closed")
	}
	return content
}
