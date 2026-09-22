package payments

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

func TestProductIDs(t *testing.T) {
	env := map[string]string{
		"PUG_DODO_PRODUCT_STARTER": "prod_s",
		"PUG_DODO_PRODUCT_GROWTH":  "prod_g",
		// No SCALE key: a tier with no product is simply not purchasable.
		"PUG_DODO_PRODUCT_CUSTOM": "prod_never_read",
		"PUG_DODO_PRODUCT_FREE":   "prod_never_read",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got, err := productIDs(lookup)
	if err != nil {
		t.Fatalf("productIDs: %v", err)
	}
	want := map[string]string{"starter": "prod_s", "growth": "prod_g"}
	if len(got) != len(want) {
		t.Fatalf("productIDs = %v, want %v", got, want)
	}
	for slug, id := range want {
		if got[slug] != id {
			t.Errorf("productIDs[%q] = %q, want %q", slug, got[slug], id)
		}
	}

	// Two tiers on one product makes an incoming subscription's tier ambiguous,
	// and the webhook would pick one silently.
	env["PUG_DODO_PRODUCT_SCALE"] = "prod_g"
	if _, err := productIDs(lookup); err == nil {
		t.Fatal("two tiers sharing a product id was accepted")
	}
}

// Only the floors and custom are excluded. A retired tier keeps its mapping, or the
// webhook could not place its existing holders' renewals and cancellations.
func TestMappedSlug(t *testing.T) {
	for _, tc := range []struct {
		plan entitlement.Plan
		want bool
	}{
		{entitlement.Plan{Slug: "growth"}, true},
		{entitlement.Plan{Slug: "growth-v0", Retired: true}, true},
		{entitlement.Plan{Slug: entitlement.SlugFree}, false},
		{entitlement.Plan{Slug: entitlement.SlugTrial}, false},
		{entitlement.Plan{Slug: entitlement.SlugCustom}, false},
	} {
		if got := mappedSlug(tc.plan); got != tc.want {
			t.Errorf("mappedSlug(%q, retired=%v) = %v, want %v",
				tc.plan.Slug, tc.plan.Retired, got, tc.want)
		}
	}
}
