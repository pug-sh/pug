package resend

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
	resendsdk "github.com/resend/resend-go/v3"
)

type Config struct {
	APIKey string `env:"PUG_RESEND_API_KEY"`
}

type Provider struct {
	client *resendsdk.Client
}

func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("resend: API key is required")
	}
	return &Provider{
		client: newClient(newObservedHTTPClient(nil), cfg.APIKey),
	}, nil
}

func (p *Provider) Send(ctx context.Context, msg emailspec.Message) error {
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

	options := &resendsdk.SendEmailOptions{IdempotencyKey: msg.IdempotencyKey}
	status := &responseStatus{}
	ctx = context.WithValue(ctx, responseStatusContextKey{}, status)
	sent, err := p.client.Emails.SendWithOptions(ctx, params, options)
	if err != nil {
		wrappedErr := fmt.Errorf("resend send email: %w", err)
		if shouldTreatAsPermanent(status.code, err) {
			return emailspec.NewPermanentError(wrappedErr)
		}
		return wrappedErr
	}
	if sent == nil || sent.Id == "" {
		return emailspec.NewPermanentError(fmt.Errorf("resend send email: empty response"))
	}
	return nil
}

type responseStatusContextKey struct{}

type responseStatus struct {
	code int
}

type observingTransport struct {
	base http.RoundTripper
}

func newObservedHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clone.Transport = observingTransport{base: transport}
	return &clone
}

func newClient(httpClient *http.Client, apiKey string) *resendsdk.Client {
	return resendsdk.NewCustomClient(httpClient, apiKey)
}

func (t observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if capture, ok := req.Context().Value(responseStatusContextKey{}).(*responseStatus); ok && resp != nil {
		capture.code = resp.StatusCode
	}
	return resp, err
}

func shouldTreatAsPermanent(statusCode int, err error) bool {
	if statusCode >= 400 && statusCode < 500 && statusCode != http.StatusTooManyRequests {
		return true
	}

	var missingFieldsErr *resendsdk.MissingRequiredFieldsError
	return errors.As(err, &missingFieldsErr)
}
