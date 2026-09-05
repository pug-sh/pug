package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
	"github.com/pug-sh/pug/internal/slogx"
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
		return nil, errors.New("resend: API key is required")
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
		// A 409 is never a plain replay — replaying a key with an identical
		// payload returns the original 200. It is one of two conflicts, and
		// only the concurrent one clears on a retry.
		if status.get() == http.StatusConflict {
			switch status.name() {
			case errNameConcurrentIdempotentRequests:
				slog.WarnContext(ctx, "resend idempotency key in flight; retrying",
					slogx.Error(err), slog.String("idempotency_key", msg.IdempotencyKey))
				return wrappedErr
			case errNameInvalidIdempotentRequest:
				return emailspec.NewPermanentError(wrappedErr)
			}
		}
		if shouldTreatAsPermanent(status.get(), err) {
			return emailspec.NewPermanentError(wrappedErr)
		}
		return wrappedErr
	}
	if sent == nil || sent.Id == "" {
		return emailspec.NewPermanentError(errors.New("resend send email: empty response"))
	}
	return nil
}

type responseStatusContextKey struct{}

// Resend's two 409 error codes. The SDK models the `name` field only for
// 400/422 (InvalidRequestError); every other status falls through to
// DefaultError, which keeps `message` alone — hence errorNameFromBody.
const (
	errNameConcurrentIdempotentRequests = "concurrent_idempotent_requests"
	errNameInvalidIdempotentRequest     = "invalid_idempotent_request"
)

// maxErrorBodyPeek bounds the error body we buffer to classify a conflict.
// Resend's error payloads are a few hundred bytes.
const maxErrorBodyPeek = 64 << 10

// responseStatus carries the HTTP status code and Resend error code from the
// RoundTrip goroutine back to the caller. The Resend SDK does the request on a
// callee goroutine and the read happens after the SDK returns, so
// happens-before is established in practice — but a future SDK refactor could
// break that assumption, so we guard the fields with a mutex.
type responseStatus struct {
	mu      sync.Mutex
	code    int
	errName string
}

func (s *responseStatus) set(code int, errName string) {
	s.mu.Lock()
	s.code = code
	s.errName = errName
	s.mu.Unlock()
}

func (s *responseStatus) get() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func (s *responseStatus) name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errName
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
	capture, ok := req.Context().Value(responseStatusContextKey{}).(*responseStatus)
	if !ok || resp == nil {
		return resp, err
	}
	errName := ""
	if resp.StatusCode == http.StatusConflict {
		errName = errorNameFromBody(resp)
	}
	capture.set(resp.StatusCode, errName)
	return resp, err
}

// errorNameFromBody reads Resend's `name` error code off the response and puts
// the body back for the SDK to parse in turn.
func errorNameFromBody(resp *http.Response) string {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyPeek))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return ""
	}
	var parsed struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Name
}

func shouldTreatAsPermanent(statusCode int, err error) bool {
	if statusCode >= 400 && statusCode < 500 && statusCode != http.StatusTooManyRequests {
		return true
	}

	var missingFieldsErr *resendsdk.MissingRequiredFieldsError
	return errors.As(err, &missingFieldsErr)
}
