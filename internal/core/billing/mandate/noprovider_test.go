package mandate_test

import (
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/mandate"
)

// The two shapes with no way to take money: no provider credentials, and the
// billing switch off. Both must refuse identically.
func noProviderCases(t *testing.T) (*fixture, map[string]*mandate.Service) {
	t.Helper()
	f, provider := newPaidFixture(t)

	unconfigured := mandate.NewService(f.pg.PgRO, f.pg.PgW, nil, f.entitlements)
	// A provider wired and the switch off, which means an entitlement service built
	// off, as the server builds it.
	off, err := entitlement.NewService(f.pg.PgRO, f.pg.PgW, false)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	switchedOff := mandate.NewService(f.pg.PgRO, f.pg.PgW, &corebilling.Payments{
		ProductBySlug: map[string]string{"growth": "prod_growth"},
		Provider:      provider,
	}, off)
	return f, map[string]*mandate.Service{
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
			if _, _, err := svc.CreateCheckoutSession(t.Context(), mandate.Checkout{
				OrgID: f.orgID, PlanSlug: "growth", Email: "buyer@example.com", Name: "Ada Buyer",
			}); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("CreateCheckoutSession err = %v, want ErrNoProvider", err)
			}
			if _, err := svc.CreatePortalSession(t.Context(), f.orgID); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("CreatePortalSession err = %v, want ErrNoProvider", err)
			}
			if _, err := svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrNoProvider) {
				t.Errorf("ConfirmCheckout err = %v, want ErrNoProvider", err)
			}
		})
	}
}

// A delivery that reaches a service with no provider has nothing to be stored or
// mapped under. MountBilling mounts no route for one, so this is wiring gone
// wrong, and it is refused for the provider's retry rather than stored orphaned.
func TestHandleDeliveryRefusesWithoutAProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)

	err := f.svc.HandleDelivery(t.Context(), delivery("evt_1", time.Now().UTC()))
	if !errors.Is(err, corebilling.ErrNoProvider) {
		t.Fatalf("HandleDelivery err = %v, want ErrNoProvider", err)
	}
	var n int
	if err := f.pg.PgRO.QueryRow(t.Context(), `select count(*) from billing_webhook_deliveries`).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 0 {
		t.Errorf("stored %d deliveries under no provider, want 0", n)
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
