package email_test

import (
	"context"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/templates"
)

func newTestRenderer() *coreemail.Renderer {
	return coreemail.NewRenderer(coreemail.Brand{
		ProductName:  "Pug",
		LogoURL:      "https://cdn.example/logo.png",
		DashboardURL: "https://app.example",
	})
}

func TestRenderMagicLinkHTML(t *testing.T) {
	r := newTestRenderer()
	html, text, err := r.MagicLink(context.Background(), "https://app.example/magic-link?token=abc")
	if err != nil {
		t.Fatalf("MagicLink: %v", err)
	}
	// premailer must have inlined the button background (brand primary color).
	if !strings.Contains(html, "background-color:"+templates.ColorPrimary) {
		t.Fatalf("button background not inlined; html=%s", html)
	}
	// The action link must be present and intact.
	if !strings.Contains(html, "https://app.example/magic-link?token=abc") {
		t.Fatalf("magic link missing from html")
	}
	// No OKLCH may ever reach the client.
	if strings.Contains(html, "oklch(") {
		t.Fatalf("oklch leaked into rendered html")
	}
	// Logo + wordmark present.
	if !strings.Contains(html, "https://cdn.example/logo.png") || !strings.Contains(html, "Pug") {
		t.Fatalf("header logo/wordmark missing")
	}
	// Plaintext twin carries the link.
	if !strings.Contains(text, "https://app.example/magic-link?token=abc") {
		t.Fatalf("plaintext missing link: %q", text)
	}
}

func TestRenderInviteWithInviter(t *testing.T) {
	r := newTestRenderer()
	html, text, err := r.Invite(context.Background(), "Acme", "Alice", "https://app.example/magic-link?token=inv")
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !strings.Contains(html, "Acme") || !strings.Contains(html, "Alice") {
		t.Fatalf("invite html missing org/inviter: %s", html)
	}
	if !strings.Contains(html, "https://app.example/magic-link?token=inv") {
		t.Fatalf("invite link missing")
	}
	if !strings.Contains(text, "Acme") || !strings.Contains(text, "https://app.example/magic-link?token=inv") {
		t.Fatalf("invite text missing org/link: %q", text)
	}
}

func TestRenderInviteWithoutInviter(t *testing.T) {
	r := newTestRenderer()
	html, _, err := r.Invite(context.Background(), "Acme", "", "https://app.example/magic-link?token=inv")
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !strings.Contains(html, "Acme") {
		t.Fatalf("invite html missing org")
	}
}

func TestRenderProviderTest(t *testing.T) {
	r := newTestRenderer()
	html, text, err := r.ProviderTest(context.Background())
	if err != nil {
		t.Fatalf("ProviderTest: %v", err)
	}
	if !strings.Contains(html, "configured correctly") {
		t.Fatalf("provider-test html missing confirmation copy: %s", html)
	}
	if !strings.Contains(text, "configured correctly") {
		t.Fatalf("provider-test text missing confirmation copy: %q", text)
	}
}

// TestRenderInviteEscapesHTMLInjection locks the feature's core safety
// contract: attacker-controlled org/inviter names must be HTML-escaped by
// templ, never emitted as live markup.
func TestRenderInviteEscapesHTMLInjection(t *testing.T) {
	r := newTestRenderer()
	const payload = `<script>alert(1)</script>`
	html, _, err := r.Invite(context.Background(), payload, payload, "https://app.example/x")
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if strings.Contains(html, payload) {
		t.Fatalf("raw script payload reached html output (XSS): %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected escaped payload in html, got: %s", html)
	}
}

// TestPlaintextTwinsCarryExpiryFacts guards the hand-written plaintext twins
// against drifting away from the expiry facts their HTML counterparts state.
func TestPlaintextTwinsCarryExpiryFacts(t *testing.T) {
	r := newTestRenderer()
	_, mlText, err := r.MagicLink(context.Background(), "https://app.example/x")
	if err != nil {
		t.Fatalf("MagicLink: %v", err)
	}
	if !strings.Contains(mlText, "expires shortly") {
		t.Fatalf("magic-link plaintext missing expiry hint: %q", mlText)
	}
	_, invText, err := r.Invite(context.Background(), "Acme", "Alice", "https://app.example/x")
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !strings.Contains(invText, "expires in 7 days") {
		t.Fatalf("invite plaintext missing expiry: %q", invText)
	}
}
