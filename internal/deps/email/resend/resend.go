package resend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

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
		if shouldTreatAsPermanent(status.get(), err) {
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

// responseStatus carries the HTTP status code from the RoundTrip goroutine
// back to the caller. The Resend SDK does the request on a callee goroutine
// and the read happens after the SDK returns, so happens-before is
// established in practice — but a future SDK refactor could break that
// assumption, so we guard the field with a mutex.
type responseStatus struct {
	mu   sync.Mutex
	code int
}

func (s *responseStatus) set(code int) {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
}

func (s *responseStatus) get() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
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
		capture.set(resp.StatusCode)
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
