package resend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resendsdk "github.com/resend/resend-go/v3"
)

func TestProviderSendSetsIdempotencyKeyHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Idempotency-Key"); got != "password_reset:reset-token" {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"email_123"}`)
	}))
	defer server.Close()

	client := resendsdk.NewCustomClient(server.Client(), "test-api-key")
	client.BaseURL = mustParseBaseURL(t, server.URL+"/")
	provider := &Provider{
		apiKey: "test-api-key",
		client: client,
	}

	err := provider.Send(context.Background(), coreemail.Message{
		IdempotencyKey: "password_reset:reset-token",
		From:           "noreply@example.com",
		To:             "test@example.com",
		Subject:        "Reset your password",
		TextBody:       "body",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func mustParseBaseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	return parsed
}
