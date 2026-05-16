package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	coreemail "github.com/pug-sh/pug/internal/core/email"
)

type Config struct {
	Provider string `env:"PUG_EMAIL_PROVIDER,default=resend"`
	APIKey   string `env:"PUG_RESEND_API_KEY"`
}

type Provider struct {
	apiKey string
	client *http.Client
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
		client: http.DefaultClient,
	}, nil
}

func (p *Provider) Send(ctx context.Context, msg coreemail.Message) error {
	body := map[string]any{
		"from":    msg.From,
		"to":      []string{msg.To},
		"subject": msg.Subject,
		"html":    msg.HTMLBody,
		"text":    msg.TextBody,
	}
	if msg.ReplyTo != "" {
		body["reply_to"] = msg.ReplyTo
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return coreemail.NewPermanentError(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return coreemail.NewPermanentError(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	msgText := strings.TrimSpace(string(respBody))
	if msgText == "" {
		msgText = resp.Status
	}
	err = fmt.Errorf("resend send email: status %d: %s", resp.StatusCode, msgText)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return err
	}
	return coreemail.NewPermanentError(err)
}
