package server

import (
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/deps/dodo"
)

func TestNewPaymentsNoProviderConfigured(t *testing.T) {
	// Billing enabled with no payments credentials is a supported mode: quotas,
	// grants and comped deals all work and only the buy button is missing.
	for name, env := range map[string]map[string]string{
		"no provider named": {"PUG_BILLING_PROVIDER": ""},
		"provider but no api key": {
			"PUG_BILLING_PROVIDER": dodo.Name,
			"PUG_DODO_API_KEY":     "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			payments, err := newPayments(t.Context())
			if err != nil {
				t.Fatalf("newPayments: %v", err)
			}
			if payments != nil {
				t.Errorf("newPayments = %v, want nil", payments)
			}
		})
	}
}

// From the dashboard, an unrecognised name and a disabled checkout are
// indistinguishable, so the name has to fail startup instead.
func TestNewPaymentsRejectsAnUnknownProvider(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if _, err := newPayments(t.Context()); err == nil {
		t.Fatal("an unknown PUG_BILLING_PROVIDER was accepted")
	}
}

// Dodo rejects a relative return_url, so a missing dashboard URL has to fail
// startup rather than every checkout.
func TestNewPaymentsRequiresAnAbsoluteDashboardURL(t *testing.T) {
	for _, base := range []string{"", "/", "app.example.com", "/settings"} {
		t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
		t.Setenv("PUG_DODO_API_KEY", "sk_test")
		t.Setenv("PUG_DASHBOARD_BASE_URL", base)
		if _, err := newPayments(t.Context()); err == nil {
			t.Errorf("PUG_DASHBOARD_BASE_URL=%q was accepted", base)
		}
	}
}

func TestNewPaymentsBuildsTheProvider(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", strings.ToUpper(dodo.Name))
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	t.Setenv("PUG_DODO_WEBHOOK_SECRET", "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	t.Setenv("PUG_DODO_PRODUCT_GROWTH", "prod_growth")
	// The trailing slash is the one a dashboard base URL is usually written with.
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://app.example.com/")

	payments, err := newPayments(t.Context())
	if err != nil {
		t.Fatalf("newPayments: %v", err)
	}
	if payments == nil {
		t.Fatal("newPayments returned no provider for a fully configured deployment")
	}
	if !payments.Provider.CanVerify() {
		t.Error("CanVerify = false with a webhook secret configured; the route would not mount")
	}
	if want := "https://app.example.com" + checkoutReturnPath; payments.ReturnURL != want {
		t.Errorf("ReturnURL = %q, want %q", payments.ReturnURL, want)
	}
	if payments.ProductBySlug["growth"] != "prod_growth" {
		t.Errorf("ProductBySlug = %v", payments.ProductBySlug)
	}
}

// The route does not mount without a secret, so every delivery 404s — but
// checkout still works, and startup must not fail.
func TestNewPaymentsWithoutAWebhookSecret(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	t.Setenv("PUG_DODO_WEBHOOK_SECRET", "")
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://app.example.com")

	payments, err := newPayments(t.Context())
	if err != nil {
		t.Fatalf("newPayments: %v", err)
	}
	if payments == nil {
		t.Fatal("newPayments returned no provider; checkout still works without a webhook secret")
	}
	if payments.Provider.CanVerify() {
		t.Error("CanVerify = true with no webhook secret; the route would mount and take unverified deliveries")
	}
}

func TestNewPaymentsRejectsAMisconfiguredCatalog(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	// One product cannot back two tiers: the webhook resolves a plan by product
	// id and would pick one silently.
	t.Setenv("PUG_DODO_PRODUCT_GROWTH", "prod_same")
	t.Setenv("PUG_DODO_PRODUCT_SCALE", "prod_same")

	if _, err := newPayments(t.Context()); err == nil {
		t.Fatal("two tiers sharing a product id was accepted")
	}
}

func TestNewPaymentsRejectsAnUnknownEnvironment(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	// A typo'd value silently pointing live traffic at the test environment takes
	// real money nowhere.
	t.Setenv("PUG_DODO_ENVIRONMENT", "staging")

	if _, err := newPayments(t.Context()); err == nil {
		t.Fatal("an unknown PUG_DODO_ENVIRONMENT was accepted")
	}
}
