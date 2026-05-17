package email

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
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

type Message struct {
	IdempotencyKey string
	From           string
	ReplyTo        string
	Subject        string
	To             string
	HTMLBody       string
	TextBody       string
}

type Provider interface {
	Send(ctx context.Context, msg Message) error
}

type PermanentError struct {
	err error
}

func NewPermanentError(err error) *PermanentError {
	if err == nil {
		panic("email: nil permanent error")
	}
	return &PermanentError{err: err}
}

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }

func IsPermanentError(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

type Service struct {
	baseURL  string
	from     string
	replyTo  string
	provider Provider
}

func NewService(cfg Config, provider Provider) (*Service, error) {
	if provider == nil {
		return nil, errors.New("email: provider is required")
	}
	baseURL := strings.TrimRight(cfg.DashboardBaseURL, "/")
	if baseURL == "" {
		return nil, errors.New("email: dashboard base URL is required")
	}
	if cfg.From == "" {
		return nil, errors.New("email: from address is required")
	}
	return &Service{
		baseURL:  baseURL,
		from:     cfg.From,
		replyTo:  cfg.ReplyTo,
		provider: provider,
	}, nil
}

func (s *Service) SendSignupVerifyWelcome(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/verify-email", token)
	return s.provider.Send(ctx, Message{
		IdempotencyKey: idempotencyKey,
		From:           s.from,
		ReplyTo:        s.replyTo,
		To:             emailAddr,
		Subject:        "Verify your email",
		TextBody:       fmt.Sprintf("Welcome to Pug.\n\nVerify your email: %s", link),
		HTMLBody:       fmt.Sprintf("<p>Welcome to Pug.</p><p><a href=\"%s\">Verify your email</a>.</p>", html.EscapeString(link)),
	})
}

func (s *Service) SendPasswordReset(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/reset-password", token)
	return s.provider.Send(ctx, Message{
		IdempotencyKey: idempotencyKey,
		From:           s.from,
		ReplyTo:        s.replyTo,
		To:             emailAddr,
		Subject:        "Reset your password",
		TextBody:       fmt.Sprintf("Reset your password: %s", link),
		HTMLBody:       fmt.Sprintf("<p><a href=\"%s\">Reset your password</a>.</p>", html.EscapeString(link)),
	})
}

func (s *Service) SendVerificationResend(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/verify-email", token)
	return s.provider.Send(ctx, Message{
		IdempotencyKey: idempotencyKey,
		From:           s.from,
		ReplyTo:        s.replyTo,
		To:             emailAddr,
		Subject:        "Verify your email",
		TextBody:       fmt.Sprintf("Verify your email: %s", link),
		HTMLBody:       fmt.Sprintf("<p><a href=\"%s\">Verify your email</a>.</p>", html.EscapeString(link)),
	})
}

func (s *Service) SendOrgMemberInvite(ctx context.Context, emailAddr, orgName, inviterName, token, idempotencyKey string) error {
	link := s.link("/accept-invite", token)
	text := fmt.Sprintf("You were invited to join %s.\n\nAccept the invite: %s", orgName, link)
	htmlBody := fmt.Sprintf("<p>You were invited to join %s.</p><p><a href=\"%s\">Accept the invite</a>.</p>", html.EscapeString(orgName), html.EscapeString(link))
	if inviterName != "" {
		text = fmt.Sprintf("%s invited you to join %s.\n\nAccept the invite: %s", inviterName, orgName, link)
		htmlBody = fmt.Sprintf("<p>%s invited you to join %s.</p><p><a href=\"%s\">Accept the invite</a>.</p>", html.EscapeString(inviterName), html.EscapeString(orgName), html.EscapeString(link))
	}
	return s.provider.Send(ctx, Message{
		IdempotencyKey: idempotencyKey,
		From:           s.from,
		ReplyTo:        s.replyTo,
		To:             emailAddr,
		Subject:        fmt.Sprintf("Invitation to join %s", orgName),
		TextBody:       text,
		HTMLBody:       htmlBody,
	})
}

func (s *Service) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.baseURL, path, url.QueryEscape(token))
}
