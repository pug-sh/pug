package email

import (
	"context"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/fallback"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	sesdeps "github.com/pug-sh/pug/internal/deps/email/ses"
)

func TestNewFallbackProviderResend(t *testing.T) {
	t.Setenv("PUG_EMAIL_PROVIDER", "resend")
	t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

	provider, err := fallback.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("fallback.NewProvider: %v", err)
	}
	if _, ok := provider.(*resenddeps.Provider); !ok {
		t.Fatalf("expected *resend.Provider, got %T", provider)
	}
}

func TestNewFallbackProviderUnsupported(t *testing.T) {
	t.Setenv("PUG_EMAIL_PROVIDER", "unknown")

	_, err := fallback.NewProvider(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `unsupported fallback provider "unknown"`) {
		t.Fatalf("expected provider name in error, got %v", err)
	}
}

func TestNewFallbackProviderSES(t *testing.T) {
	t.Setenv("PUG_EMAIL_PROVIDER", "ses")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	provider, err := fallback.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("fallback.NewProvider: %v", err)
	}
	if _, ok := provider.(*sesdeps.Provider); !ok {
		t.Fatalf("expected *ses.Provider, got %T", provider)
	}
}

func TestNewMailerUsesOperatorOnlyWhenNoKey(t *testing.T) {
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
	t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
	t.Setenv("PUG_EMAIL_PROVIDER_SECRET_KEY", "")

	originalFactory := fallbackProviderFactory
	t.Cleanup(func() { fallbackProviderFactory = originalFactory })

	called := false
	fallbackProviderFactory = func(context.Context) (coreemail.Provider, error) {
		called = true
		return &fakeProvider{}, nil
	}

	mailer, err := newMailerWithResolver(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("newMailerWithResolver: %v", err)
	}
	if !called {
		t.Fatal("expected fallback factory to be called")
	}
	if mailer == nil {
		t.Fatal("expected mailer")
	}
}
