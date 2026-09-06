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
			payments, canVerify, err := newPayments(t.Context())
			if err != nil {
				t.Fatalf("newPayments: %v", err)
			}
			if payments != nil || canVerify {
				t.Errorf("newPayments = (%v, %v), want (nil, false)", payments, canVerify)
			}
		})
	}
}

// From the dashboard, an unrecognised name and a disabled checkout are
// indistinguishable, so the name has to fail startup instead.
func TestNewPaymentsRejectsAnUnknownProvider(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if _, _, err := newPayments(t.Context()); err == nil {
		t.Fatal("an unknown PUG_BILLING_PROVIDER was accepted")
	}
}

func TestNewPaymentsBuildsTheProvider(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", strings.ToUpper(dodo.Name))
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	t.Setenv("PUG_DODO_WEBHOOK_SECRET", "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	t.Setenv("PUG_DODO_PRODUCT_GROWTH", "prod_growth")
	// The trailing slash is the one a dashboard base URL is usually written with.
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://app.example.com/")

	payments, canVerify, err := newPayments(t.Context())
	if err != nil {
		t.Fatalf("newPayments: %v", err)
	}
	if payments == nil {
		t.Fatal("newPayments returned no provider for a fully configured deployment")
	}
	if !canVerify {
		t.Error("canVerify = false with a webhook secret configured; the route would not mount")
	}
	if want := "https://app.example.com" + checkoutReturnPath; payments.ReturnURL != want {
		t.Errorf("ReturnURL = %q, want %q", payments.ReturnURL, want)
	}
	if payments.ProductBySlug["growth"] != "prod_growth" {
		t.Errorf("ProductBySlug = %v", payments.ProductBySlug)
	}
	// Both directions come from one map so checkout and the webhook cannot
	// disagree about which tier a product is.
	if payments.SlugByProduct["prod_growth"] != "growth" {
		t.Errorf("SlugByProduct = %v", payments.SlugByProduct)
	}
}

// The route does not mount without a secret, so every delivery 404s -- but
// checkout still works, and startup must not fail.
func TestNewPaymentsWithoutAWebhookSecret(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	t.Setenv("PUG_DODO_WEBHOOK_SECRET", "")

	payments, canVerify, err := newPayments(t.Context())
	if err != nil {
		t.Fatalf("newPayments: %v", err)
	}
	if payments == nil || canVerify {
		t.Errorf("newPayments = (%v, %v), want a provider that cannot verify", payments, canVerify)
	}
}

func TestNewPaymentsRejectsAMisconfiguredCatalog(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	// One product cannot back two tiers: the webhook resolves a plan by product
	// id and would pick one silently.
	t.Setenv("PUG_DODO_PRODUCT_GROWTH", "prod_same")
	t.Setenv("PUG_DODO_PRODUCT_SCALE", "prod_same")

	if _, _, err := newPayments(t.Context()); err == nil {
		t.Fatal("two tiers sharing a product id was accepted")
	}
}

func TestNewPaymentsRejectsAnUnknownEnvironment(t *testing.T) {
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	// A typo'd value silently pointing live traffic at the test environment takes
	// real money nowhere.
	t.Setenv("PUG_DODO_ENVIRONMENT", "staging")

	if _, _, err := newPayments(t.Context()); err == nil {
		t.Fatal("an unknown PUG_DODO_ENVIRONMENT was accepted")
	}
}
