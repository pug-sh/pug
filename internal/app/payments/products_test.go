package payments

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

func TestProductIDs(t *testing.T) {
	env := map[string]string{
		"PUG_DODO_PRODUCT_USAGE_2026_10": "prod_u",
		// Free and custom are states rather than catalog plans: nothing reads these.
		"PUG_DODO_PRODUCT_CUSTOM": "prod_never_read",
		"PUG_DODO_PRODUCT_FREE":   "prod_never_read",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got, err := productIDs(lookup, entitlement.Plans())
	if err != nil {
		t.Fatalf("productIDs: %v", err)
	}
	if len(got) != 1 || got[entitlement.SlugUsage] != "prod_u" {
		t.Fatalf("productIDs = %v, want {%s: prod_u}", got, entitlement.SlugUsage)
	}

	// The real catalog holds one plan, so the rest of this test makes its own.
	plans := []entitlement.Plan{
		{Slug: "usage-old", Retired: true},
		{Slug: "usage-new"},
		{Slug: "usage-unsold"},
	}
	env["PUG_DODO_PRODUCT_USAGE_OLD"] = "prod_old"
	env["PUG_DODO_PRODUCT_USAGE_NEW"] = "prod_new"
	got, err = productIDs(lookup, plans)
	if err != nil {
		t.Fatalf("productIDs: %v", err)
	}
	// A retired plan keeps its mapping, or the webhook would reject its holders'
	// renewals; a cancellation lands either way, on the plan slug already stored. A
	// plan on sale with no key is not purchasable.
	if got["usage-old"] != "prod_old" || got["usage-new"] != "prod_new" || len(got) != 2 {
		t.Errorf("productIDs = %v, want the retired and the new plan mapped", got)
	}

	// Two plans on one product makes an incoming subscription's plan ambiguous, and
	// the webhook would pick one silently.
	env["PUG_DODO_PRODUCT_USAGE_UNSOLD"] = "prod_new"
	if _, err := productIDs(lookup, plans); err == nil {
		t.Fatal("two plans sharing a product id was accepted")
	}
}
