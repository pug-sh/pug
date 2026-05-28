package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
)

func TestFormatInfraHealth(t *testing.T) {
	if got := formatInfraHealth(nil); got != green+"connected"+reset {
		t.Fatalf("connected status = %q", got)
	}

	errMsg := formatInfraHealth(errors.New("connection refused"))
	if !strings.Contains(errMsg, red) || !strings.Contains(errMsg, "connection refused") {
		t.Fatalf("error status = %q", errMsg)
	}
}

func TestShortProbeError(t *testing.T) {
	if got := shortProbeError(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline = %q, want timeout", got)
	}

	long := strings.Repeat("x", 100)
	if got := shortProbeError(errors.New(long)); len(got) != 80 {
		t.Fatalf("truncated length = %d, want 80", len(got))
	}

	if got := shortProbeError(errors.New("first line\nsecond line")); got != "first line" {
		t.Fatalf("first line only = %q", got)
	}
}

func TestEmailDevStatus(t *testing.T) {
	t.Run("missing dashboard base URL", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := "disabled (missing PUG_DASHBOARD_BASE_URL)"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("default resend requires API key", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "")
		t.Setenv("PUG_RESEND_API_KEY", "")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := "disabled (missing PUG_RESEND_API_KEY for resend)"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("resend enabled when configured", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "resend")
		t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

		enabled, status := emailDevStatus()
		if !enabled {
			t.Fatal("expected email worker to be enabled")
		}
		if want := "email"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("ses enabled without app-specific credentials", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "ses")
		t.Setenv("PUG_RESEND_API_KEY", "")

		enabled, status := emailDevStatus()
		if !enabled {
			t.Fatal("expected email worker to be enabled")
		}
		if want := "email"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("unsupported provider is disabled", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "mailgun")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := `disabled (unsupported provider "mailgun")`; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})
}

func TestRenderEmailPreviewKinds(t *testing.T) {
	r := coreemail.NewRenderer(coreemail.Brand{ProductName: "Pug", DashboardURL: "https://app.example"})
	for _, kind := range []string{"magic_link", "invite", "provider_test"} {
		html, text, err := renderEmailPreview(context.Background(), r, kind, "https://app.example/x")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if html == "" || text == "" {
			t.Fatalf("%s: empty render (html=%d bytes, text=%d bytes)", kind, len(html), len(text))
		}
	}
}

func TestRenderEmailPreviewUnknownKind(t *testing.T) {
	r := coreemail.NewRenderer(coreemail.Brand{ProductName: "Pug"})
	if _, _, err := renderEmailPreview(context.Background(), r, "bogus", "https://app.example/x"); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
