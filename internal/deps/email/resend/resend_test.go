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

// TestProviderSendEmptyResponseIsPermanent pins resend.go:53. A 200 with an
// empty id is anomalous — likely an API surface drift or a proxy stripping
// the body. Retrying won't help, so the worker must DLQ.
func TestProviderSendEmptyResponseIsPermanent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_id", `{"id":""}`},
		{"absent_id", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProvider(t, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, tc.body), nil
			}))
			err := provider.Send(context.Background(), emailspec.Message{
				From:     "noreply@example.com",
				To:       "test@example.com",
				Subject:  "Subject",
				TextBody: "body",
			})
			if err == nil {
				t.Fatal("expected error on empty response id")
			}
			if !emailspec.IsPermanentError(err) {
				t.Fatalf("expected permanent error so worker DLQs, got %v", err)
			}
		})
	}
}

// TestProviderSendSplitsIdempotencyConflicts pins the two 409 flavors apart. A
// replay with an identical payload returns the original 200, so a 409 never
// means "already sent": a concurrent send clears on retry, a payload mismatch
// never will, and an unrecognized code falls back to 4xx-is-permanent.
//
// wantMessage also pins that errorNameFromBody restores the body — consuming it
// would leave the SDK decoding nothing and reporting a bare "Conflict".
func TestProviderSendSplitsIdempotencyConflicts(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		permanent   bool
		wantMessage string
	}{
		{
			name:        "concurrent_is_retryable",
			body:        `{"name":"concurrent_idempotent_requests","message":"request in progress"}`,
			permanent:   false,
			wantMessage: "request in progress",
		},
		{
			name:        "payload_mismatch_is_permanent",
			body:        `{"name":"invalid_idempotent_request","message":"same key, different payload"}`,
			permanent:   true,
			wantMessage: "same key, different payload",
		},
		{
			name:        "unknown_code_is_permanent",
			body:        `{"message":"conflict"}`,
			permanent:   true,
			wantMessage: "conflict",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProvider(t, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusConflict, tc.body), nil
			}))

			err := provider.Send(context.Background(), emailspec.Message{
				From:           "noreply@example.com",
				To:             "test@example.com",
				Subject:        "Subject",
				TextBody:       "body",
				IdempotencyKey: "dispatch-1",
			})
			if err == nil {
				t.Fatal("expected 409 to surface an error")
			}
			if got := emailspec.IsPermanentError(err); got != tc.permanent {
				t.Fatalf("permanent = %v, want %v (err: %v)", got, tc.permanent, err)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error %q lost the provider message %q", err, tc.wantMessage)
			}
		})
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
