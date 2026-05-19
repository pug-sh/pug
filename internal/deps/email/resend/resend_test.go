package resend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
	resendsdk "github.com/resend/resend-go/v3"
)

func TestNewRequiresAPIKey(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProviderSendWrapsClientErrorsAsPermanent(t *testing.T) {
	provider := newTestProvider(t, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"message":"invalid api key"}`), nil
	}))

	err := provider.Send(context.Background(), emailspec.Message{
		From:     "noreply@example.com",
		To:       "test@example.com",
		Subject:  "Subject",
		TextBody: "body",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !emailspec.IsPermanentError(err) {
		t.Fatalf("expected permanent error, got %T: %v", err, err)
	}
}

func TestProviderSendKeepsRateLimitsRetryable(t *testing.T) {
	provider := newTestProvider(t, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, `{"message":"rate limited"}`), nil
	}))

	err := provider.Send(context.Background(), emailspec.Message{
		From:     "noreply@example.com",
		To:       "test@example.com",
		Subject:  "Subject",
		TextBody: "body",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if emailspec.IsPermanentError(err) {
		t.Fatalf("expected retryable error, got %T: %v", err, err)
	}
	if !errors.Is(err, resendsdk.ErrRateLimit) {
		t.Fatalf("expected rate limit error, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestProvider(t *testing.T, transport http.RoundTripper) *Provider {
	t.Helper()

	httpClient := &http.Client{Transport: transport}
	client := newClient(newObservedHTTPClient(httpClient), "test-api-key")
	baseURL, err := url.Parse("https://api.test/")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	client.BaseURL = baseURL

	return &Provider{
		client: client,
	}
}

func jsonResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}
