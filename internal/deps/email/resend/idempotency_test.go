package resend

import (
	"context"
	"net/http"
	"testing"

	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
)

func TestProviderSendSetsIdempotencyKeyHeader(t *testing.T) {
	var called bool
	provider := newTestProvider(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Idempotency-Key"); got != "magic_link:tok" {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		called = true
		return jsonResponse(http.StatusOK, `{"id":"email_123"}`), nil
	}))

	err := provider.Send(context.Background(), emailspec.Message{
		IdempotencyKey: "magic_link:tok",
		From:           "noreply@example.com",
		To:             "test@example.com",
		Subject:        "Your sign-in link",
		TextBody:       "body",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !called {
		t.Fatal("expected request to be sent")
	}
}
