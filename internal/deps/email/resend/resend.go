package resend

import (
	"context"
	"fmt"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resendsdk "github.com/resend/resend-go/v3"
)

type Config struct {
	Provider string `env:"PUG_EMAIL_PROVIDER,default=resend"`
	APIKey   string `env:"PUG_RESEND_API_KEY"`
}

type Provider struct {
	apiKey string
	client *resendsdk.Client
}

func New(cfg Config) (*Provider, error) {
	if cfg.Provider != "resend" {
		return nil, fmt.Errorf("resend: unsupported email provider %q", cfg.Provider)
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("resend: API key is required")
	}
	return &Provider{
		apiKey: cfg.APIKey,
		client: resendsdk.NewClient(cfg.APIKey),
	}, nil
}

func (p *Provider) Send(ctx context.Context, msg coreemail.Message) error {
	params := &resendsdk.SendEmailRequest{
		From:    msg.From,
		To:      []string{msg.To},
		Subject: msg.Subject,
		Html:    msg.HTMLBody,
		Text:    msg.TextBody,
	}
	if msg.ReplyTo != "" {
		params.ReplyTo = msg.ReplyTo
	}

	sent, err := p.client.Emails.SendWithContext(ctx, params)
	if err != nil {
		return fmt.Errorf("resend send email: %w", err)
	}
	if sent == nil || sent.Id == "" {
		return coreemail.NewPermanentError(fmt.Errorf("resend send email: empty response"))
	}
	return nil
}
