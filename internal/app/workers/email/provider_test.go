package email

import (
	"context"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
)

func TestNewProviderResend(t *testing.T) {
	t.Setenv("PUG_EMAIL_PROVIDER", "resend")
	t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

	provider, err := newProvider(context.Background())
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	if _, ok := provider.(*resenddeps.Provider); !ok {
		t.Fatalf("expected *resend.Provider, got %T", provider)
	}
}

func TestNewProviderUnsupportedProvider(t *testing.T) {
	t.Setenv("PUG_EMAIL_PROVIDER", "ses")

	_, err := newProvider(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `unsupported provider "ses"`) {
		t.Fatalf("expected provider name in error, got %v", err)
	}
}

func TestNewMailerUsesProviderFactory(t *testing.T) {
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
	t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")

	originalFactory := providerFactory
	t.Cleanup(func() {
		providerFactory = originalFactory
	})

	called := false
	providerFactory = func(context.Context) (coreemail.Provider, error) {
		called = true
		return &fakeProvider{}, nil
	}

	mailer, err := newMailer(context.Background())
	if err != nil {
		t.Fatalf("newMailer: %v", err)
	}
	if !called {
		t.Fatal("expected provider factory to be called")
	}
	if mailer == nil {
		t.Fatal("expected mailer")
	}
}
