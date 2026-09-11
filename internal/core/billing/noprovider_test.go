package billing_test

import (
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// The two shapes with no way to take money: no provider credentials, and the
// billing switch off. Both must refuse identically.
func noProviderCases(t *testing.T) (*fixture, map[string]*corebilling.Service) {
	t.Helper()
	f, provider := newPaidFixture(t)

	unconfigured, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, corebilling.Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service with no payments: %v", err)
	}
	switchedOff, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, corebilling.Config{Enabled: false}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       provider,
	})
	if err != nil {
		t.Fatalf("new service with billing off: %v", err)
	}
	return f, map[string]*corebilling.Service{
		"no provider credentials": unconfigured,
		"billing switched off":    switchedOff,
	}
}

func TestMoneyPathsRefuseWithNoProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, services := noProviderCases(t)

	for name, svc := range services {
		t.Run(name, func(t *testing.T) {
			if _, _, err := svc.CreateCheckoutSession(t.Context(), corebilling.Checkout{
				OrgID: f.orgID, PlanSlug: corebilling.CurrentSlug, Email: "buyer@example.com", Name: "Ada Buyer",
			}); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("CreateCheckoutSession err = %v, want ErrNoProvider", err)
			}
			if _, err := svc.CreatePortalSession(t.Context(), f.orgID); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("CreatePortalSession err = %v, want ErrNoProvider", err)
			}
			if _, err := svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("ConfirmCheckout err = %v, want ErrNoProvider", err)
			}
			if err := svc.RemovePaymentMethod(t.Context(), f.orgID, time.Now()); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("RemovePaymentMethod err = %v, want ErrNoProvider", err)
			}
		})
	}
}

// Listed but not purchasable, rather than an empty catalog: a price with no
// button is the honest render for a self-hosted install.
func TestPlanOptionsAreListedButNotPurchasableWithNoProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, services := noProviderCases(t)

	for name, svc := range services {
		t.Run(name, func(t *testing.T) {
			options, err := svc.PlanOptions(t.Context(), f.orgID)
			if err != nil {
				t.Fatalf("PlanOptions: %v", err)
			}
			if len(options) == 0 {
				t.Fatal("no plans listed; the pricing page has nothing to render")
			}
			for _, opt := range options {
				if opt.Purchasable {
					t.Errorf("%s is purchasable with no provider", opt.Slug)
				}
			}
		})
	}
}
