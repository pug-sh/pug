package main

import "testing"

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
