package email

import (
	"bytes"
	"context"
	"fmt"

	"github.com/a-h/templ"
	"github.com/pug-sh/pug/internal/core/email/templates"
	"github.com/vanng822/go-premailer/premailer"
)

// Brand re-exports the presentation type so callers configure branding without
// importing the templates package (mirrors `type Message = spec.Message`).
type Brand = templates.Brand

// Renderer turns typed templ components into email-safe HTML (CSS inlined by
// go-premailer) plus a hand-written plaintext twin.
type Renderer struct {
	brand templates.Brand
}

func NewRenderer(b templates.Brand) *Renderer {
	return &Renderer{brand: b}
}

// inline renders a templ component to HTML and inlines its CSS. premailer's
// defaults (RemoveClasses=false, CssToAttributes=true) keep media queries and
// mirror CSS to Outlook-friendly attributes, which is what email needs.
func (r *Renderer) inline(ctx context.Context, c templ.Component) (string, error) {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return "", fmt.Errorf("render email template: %w", err)
	}
	prem, err := premailer.NewPremailerFromString(buf.String(), premailer.NewOptions())
	if err != nil {
		return "", fmt.Errorf("init premailer: %w", err)
	}
	html, err := prem.Transform()
	if err != nil {
		return "", fmt.Errorf("inline email css: %w", err)
	}
	return html, nil
}

// MagicLink renders the sign-in email (HTML + plaintext).
func (r *Renderer) MagicLink(ctx context.Context, link string) (html, text string, err error) {
	html, err = r.inline(ctx, templates.MagicLink(r.brand, link))
	if err != nil {
		return "", "", err
	}
	return html, magicLinkText(r.brand, link), nil
}

// Invite renders the org-member invite email. orgName and inviterName must be
// pre-sanitized by the caller (control chars stripped); templ handles HTML
// escaping.
func (r *Renderer) Invite(ctx context.Context, orgName, inviterName, link string) (html, text string, err error) {
	html, err = r.inline(ctx, templates.Invite(r.brand, orgName, inviterName, link))
	if err != nil {
		return "", "", err
	}
	return html, inviteText(r.brand, orgName, inviterName, link), nil
}

// ProviderTest renders the provider verification email.
func (r *Renderer) ProviderTest(ctx context.Context) (html, text string, err error) {
	html, err = r.inline(ctx, templates.ProviderTest(r.brand))
	if err != nil {
		return "", "", err
	}
	return html, providerTestText(r.brand), nil
}
