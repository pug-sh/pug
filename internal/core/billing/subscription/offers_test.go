package subscription

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// stubProvider wires a provider with no behaviour: offers only asks whether one is
// there.
type stubProvider struct{ billing.PaymentProvider }

var (
	onSale = entitlement.Plan{
		Slug: "usage-2027-01", DisplayName: "New", FreeEvents: 10, TierUpTo: []int64{20}, RetentionDays: 365,
	}
	retired = entitlement.Plan{
		Slug: "usage-2026-10", DisplayName: "Old", FreeEvents: 10, TierUpTo: []int64{20}, RetentionDays: 365,
		Retired: true,
	}
)

func offerService(t *testing.T, billingEnabled bool, products map[string]string) *Service {
	t.Helper()
	// Construction touches neither pool, and offers reads neither.
	entitlements, err := entitlement.NewService(nil, nil, billingEnabled)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	return NewService(nil, nil, &billing.Payments{Provider: stubProvider{}, ProductBySlug: products}, entitlements)
}

// A reprice keeps the retired plan's product mapped, so its holders' renewals still
// resolve. offers is what keeps it off sale, for the button and the checkout alike.
func TestARetiredPlanIsNeverOffered(t *testing.T) {
	plans := []entitlement.Plan{retired, onSale}

	got := offerService(t, true, map[string]string{onSale.Slug: "prod_new", retired.Slug: "prod_old"}).
		offersFrom(plans, entitlement.Record{})
	if len(got) != 1 || got[0].Slug != onSale.Slug || !got[0].Purchasable || got[0].productID != "prod_new" {
		t.Fatalf("offers = %+v, want only the plan on sale, bought against its own product", got)
	}

	for _, o := range offerService(t, true, map[string]string{retired.Slug: "prod_old"}).offersFrom(plans, entitlement.Record{}) {
		if o.Purchasable {
			t.Errorf("%s is purchasable when only a retired plan has a product", o.Slug)
		}
	}
}

// The org's own deal is offered beside the plans, from its row — and bought only
// while the deployment takes money: with billing off, a deal's product must not
// light a button that checkout then refuses.
func TestADealIsOfferedFromItsOwnRow(t *testing.T) {
	deal := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal", BasePlanSlug: onSale.Slug,
	}
	for _, tc := range []struct {
		name           string
		billingEnabled bool
	}{
		{"billing on", true},
		{"billing off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var custom *PlanOption
			for _, o := range offerService(t, tc.billingEnabled, nil).offersFrom([]entitlement.Plan{onSale}, deal) {
				if o.Slug == entitlement.SlugCustom {
					custom = &o
				}
			}
			if custom == nil {
				t.Fatal("the org's own deal is not offered")
			}
			if custom.Purchasable != tc.billingEnabled || (custom.productID == "prod_deal") != tc.billingEnabled {
				t.Errorf("deal offer = %+v, want purchasable exactly while billing is on", *custom)
			}
		})
	}

	for _, o := range offerService(t, true, nil).offersFrom([]entitlement.Plan{onSale}, entitlement.Record{}) {
		if o.Slug == entitlement.SlugCustom {
			t.Error("a deal is offered to an org whose row names no product")
		}
	}
}
