package resend

import (
	"context"
	"net/http"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
)

func TestProviderSendSetsIdempotencyKeyHeader(t *testing.T) {
	var called bool
	provider := newTestProvider(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Idempotency-Key"); got != "password_reset:reset-token" {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		called = true
		return jsonResponse(http.StatusOK, `{"id":"email_123"}`), nil
	}))

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
	if !called {
		t.Fatal("expected request to be sent")
	}
}
