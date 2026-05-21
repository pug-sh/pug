package email

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/pug-sh/pug/internal/core/email/spec"
	"github.com/pug-sh/pug/internal/core/email/templates"
)

type Config struct {
	DashboardBaseURL string `env:"PUG_DASHBOARD_BASE_URL,required"`
	From             string `env:"PUG_EMAIL_FROM,required"`
	ReplyTo          string `env:"PUG_EMAIL_REPLY_TO"`
	LogoURL          string `env:"PUG_EMAIL_LOGO_URL"`
}

// Message, Provider, PermanentError, and the permanent-error helpers are
// re-exported from spec so internal/deps/email/* can implement the Provider
// contract without importing core/email — core/email constructs per-tenant
// providers from these types, which would otherwise create an import cycle.
type Message = spec.Message

type Provider = spec.Provider

type PermanentError = spec.PermanentError

func NewPermanentError(err error) *PermanentError { return spec.NewPermanentError(err) }

func IsPermanentError(err error) bool { return spec.IsPermanentError(err) }

type Service struct {
	baseURL         string
	operatorFrom    string
	operatorReplyTo string
	resolver        ProviderResolver
	renderer        *Renderer
}

// NewService wraps a single Provider in an OperatorOnlyResolver. Use this
// when callers don't need per-tenant routing; callers that do need it should
// use NewServiceWithResolver directly.
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
		renderer: NewRenderer(Brand{
			ProductName:  templates.ProductName,
			LogoURL:      cfg.LogoURL,
			DashboardURL: baseURL,
		}),
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

func (s *Service) SendMagicLink(ctx context.Context, emailAddr, token, idempotencyKey string) error {
	link := s.link("/magic-link", token)
	htmlBody, text, err := s.renderer.MagicLink(ctx, link)
	if err != nil {
		return err
	}
	msg := s.baseMessage(idempotencyKey, emailAddr)
	msg.Subject = "Your sign-in link"
	msg.TextBody = text
	msg.HTMLBody = htmlBody
	return s.send(ctx, nil, msg)
}

func (s *Service) SendOrgMemberInvite(ctx context.Context, orgID, emailAddr, orgName, inviterName, token, idempotencyKey string) error {
	link := s.link("/magic-link", token)
	safeOrg := sanitizeDisplay(orgName)
	safeInviter := sanitizeDisplay(inviterName)
	htmlBody, text, err := s.renderer.Invite(ctx, safeOrg, safeInviter, link)
	if err != nil {
		return err
	}
	msg := s.baseMessage(idempotencyKey, emailAddr)
	if safeInviter != "" {
		msg.Subject = fmt.Sprintf("%s invited you to join %s", safeInviter, safeOrg)
	} else {
		msg.Subject = fmt.Sprintf("Invitation to join %s", safeOrg)
	}
	msg.TextBody = text
	msg.HTMLBody = htmlBody
	tenantID := orgID
	return s.send(ctx, &tenantID, msg)
}

// sanitizeDisplay strips control characters and caps length so an attacker-set
// display name can't smuggle line breaks into text bodies or weird characters
// into subjects. The SMTP layer also strips CRLF from headers as a final
// defense; this is the application-layer sanitization for content sites.
func sanitizeDisplay(s string) string {
	const maxLen = 120
	stripped := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == 0x7f || (r < 0x20 && r != '\t') {
			return -1
		}
		return r
	}, s)
	if len(stripped) > maxLen {
		// Back off to a rune boundary so a multi-byte rune straddling the cap
		// isn't split into invalid UTF-8 (which would corrupt subjects/bodies).
		cut := maxLen
		for cut > 0 && !utf8.RuneStart(stripped[cut]) {
			cut--
		}
		stripped = stripped[:cut]
	}
	return stripped
}

// SendTest routes through the operator-default provider when orgID is empty.
func (s *Service) SendTest(ctx context.Context, orgID, recipient, idempotencyKey string) error {
	htmlBody, text, err := s.renderer.ProviderTest(ctx)
	if err != nil {
		return err
	}
	msg := s.baseMessage(idempotencyKey, recipient)
	msg.Subject = "Pug email provider test"
	msg.TextBody = text
	msg.HTMLBody = htmlBody
	var tenant *string
	if orgID != "" {
		tenant = &orgID
	}
	return s.send(ctx, tenant, msg)
}

func (s *Service) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.baseURL, path, url.QueryEscape(token))
}
