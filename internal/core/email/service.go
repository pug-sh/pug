package email

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/pug-sh/pug/internal/core/email/spec"
)

const (
	TemplateSignupVerifyWelcome = "signup_verify_welcome"
	TemplatePasswordReset       = "password_reset"
	TemplateOrgMemberInvite     = "org_member_invite"
)

type Config struct {
	DashboardBaseURL string `env:"PUG_DASHBOARD_BASE_URL,required"`
	From             string `env:"PUG_EMAIL_FROM,required"`
	ReplyTo          string `env:"PUG_EMAIL_REPLY_TO"`
}

// Message, Provider, PermanentError and the permanent-error helpers are
// re-exported from internal/core/email/spec so internal/deps/email/* can
// implement the Provider contract without importing core/email (cycle when
// core/email constructs per-tenant providers from spec types).
type Message = spec.Message

type Provider = spec.Provider

type PermanentError = spec.PermanentError

// NewPermanentError marks an error as non-retryable. See spec.NewPermanentError.
func NewPermanentError(err error) *PermanentError { return spec.NewPermanentError(err) }

// IsPermanentError reports whether err (or anything it wraps) is a PermanentError.
func IsPermanentError(err error) bool { return spec.IsPermanentError(err) }

type Service struct {
	baseURL         string
	operatorFrom    string
	operatorReplyTo string
	resolver        ProviderResolver
}

// NewService preserves the pre-resolver call shape by wrapping the single
// provider in an OperatorOnlyResolver. Used by tests and call sites that
// don't need per-tenant routing.
func NewService(cfg Config, provider Provider) (*Service, error) {
	if provider == nil {
		return nil, errors.New("email: provider is required")
	}
	return NewServiceWithResolver(cfg, &OperatorOnlyResolver{
		Provider: provider,
		From:     cfg.From,
		ReplyTo:  cfg.ReplyTo,
	})
}

func NewServiceWithResolver(cfg Config, resolver ProviderResolver) (*Service, error) {
	if resolver == nil {
		return nil, errors.New("email: resolver is required")
	}
	baseURL := strings.TrimRight(cfg.DashboardBaseURL, "/")
	if baseURL == "" {
		return nil, errors.New("email: dashboard base URL is required")
	}
	if cfg.From == "" {
		return nil, errors.New("email: from address is required")
	}
	return &Service{
		baseURL:         baseURL,
		operatorFrom:    cfg.From,
		operatorReplyTo: cfg.ReplyTo,
		resolver:        resolver,
	}, nil
}

func (s *Service) send(ctx context.Context, tenantID *string, msg Message) error {
	provider, override, err := s.resolver.Resolve(ctx, tenantID)
	if err != nil {
		return err
	}
	if override.From != "" {
		msg.From = override.From
	}
	if override.ReplyTo != "" {
		msg.ReplyTo = override.ReplyTo
	}
	return provider.Send(ctx, msg)
}

func (s *Service) baseMessage(idempotencyKey, to string) Message {
	return Message{
		IdempotencyKey: idempotencyKey,
		From:           s.operatorFrom,
		ReplyTo:        s.operatorReplyTo,
		To:             to,
	}
}

func (s *Service) SendSignupVerifyWelcome(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/verify-email", token)
	msg := s.baseMessage(idempotencyKey, emailAddr)
	msg.Subject = "Verify your email"
	msg.TextBody = fmt.Sprintf("Welcome to Pug.\n\nVerify your email: %s", link)
	msg.HTMLBody = fmt.Sprintf("<p>Welcome to Pug.</p><p><a href=\"%s\">Verify your email</a>.</p>", html.EscapeString(link))
	return s.send(ctx, nil, msg)
}

func (s *Service) SendPasswordReset(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/reset-password", token)
	msg := s.baseMessage(idempotencyKey, emailAddr)
	msg.Subject = "Reset your password"
	msg.TextBody = fmt.Sprintf("Reset your password: %s", link)
	msg.HTMLBody = fmt.Sprintf("<p><a href=\"%s\">Reset your password</a>.</p>", html.EscapeString(link))
	return s.send(ctx, nil, msg)
}

func (s *Service) SendVerificationResend(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/verify-email", token)
	msg := s.baseMessage(idempotencyKey, emailAddr)
	msg.Subject = "Verify your email"
	msg.TextBody = fmt.Sprintf("Verify your email: %s", link)
	msg.HTMLBody = fmt.Sprintf("<p><a href=\"%s\">Verify your email</a>.</p>", html.EscapeString(link))
	return s.send(ctx, nil, msg)
}

func (s *Service) SendOrgMemberInvite(ctx context.Context, orgID, emailAddr, orgName, inviterName, token, idempotencyKey string) error {
	link := s.link("/accept-invite", token)
	text := fmt.Sprintf("You were invited to join %s.\n\nAccept the invite: %s", orgName, link)
	htmlBody := fmt.Sprintf("<p>You were invited to join %s.</p><p><a href=\"%s\">Accept the invite</a>.</p>", html.EscapeString(orgName), html.EscapeString(link))
	if inviterName != "" {
		text = fmt.Sprintf("%s invited you to join %s.\n\nAccept the invite: %s", inviterName, orgName, link)
		htmlBody = fmt.Sprintf("<p>%s invited you to join %s.</p><p><a href=\"%s\">Accept the invite</a>.</p>", html.EscapeString(inviterName), html.EscapeString(orgName), html.EscapeString(link))
	}
	msg := s.baseMessage(idempotencyKey, emailAddr)
	msg.Subject = fmt.Sprintf("Invitation to join %s", orgName)
	msg.TextBody = text
	msg.HTMLBody = htmlBody
	tenantID := orgID
	return s.send(ctx, &tenantID, msg)
}

// SendTest sends a fixed test message via the resolver for the supplied tenant.
// Used by the dashboard SendTestEmail RPC; callers pass orgID for tenant-scoped
// testing. Empty orgID resolves against the operator default.
func (s *Service) SendTest(ctx context.Context, orgID, recipient, idempotencyKey string) error {
	msg := s.baseMessage(idempotencyKey, recipient)
	msg.Subject = "Pug email provider test"
	msg.TextBody = "This is a test email from Pug to verify your email provider configuration."
	msg.HTMLBody = "<p>This is a test email from Pug to verify your email provider configuration.</p>"
	var tenant *string
	if orgID != "" {
		tenant = &orgID
	}
	return s.send(ctx, tenant, msg)
}

func (s *Service) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.baseURL, path, url.QueryEscape(token))
}
