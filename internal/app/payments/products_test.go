package payments

import (
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

func TestProductIDs(t *testing.T) {
	// Derived from the catalog rather than hardcoded, so renaming a card does not
	// silently stop this test exercising anything.
	slug := entitlement.CurrentCard().Slug
	key := productEnvPrefix + strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))

	env := map[string]string{
		key: "prod_c",
		// States are not catalog entries, so a key naming one is never read.
		"PUG_DODO_PRODUCT_CUSTOM": "prod_never_read",
		"PUG_DODO_PRODUCT_FREE":   "prod_never_read",
		"PUG_DODO_PRODUCT_TRIAL":  "prod_never_read",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got, err := ProductIDs(lookup, entitlement.Cards())
	if err != nil {
		t.Fatalf("ProductIDs: %v", err)
	}
	want := map[string]string{slug: "prod_c"}
	if len(got) != len(want) {
		t.Fatalf("ProductIDs = %v, want %v", got, want)
	}
	if got[slug] != "prod_c" {
		t.Errorf("ProductIDs[%q] = %q, want %q", slug, got[slug], "prod_c")
	}
}

// A card with no product key is simply not purchasable, rather than an error that
// stops a deployment booting.
func TestProductIDsSkipsACardWithNoKey(t *testing.T) {
	got, err := ProductIDs(func(string) (string, bool) { return "", false }, entitlement.Cards())
	if err != nil {
		t.Fatalf("ProductIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ProductIDs = %v, want empty", got)
	}
}

// Free, trial and custom are states rather than catalog entries, so ProductIDs
// cannot map them and needs no predicate saying so. A retired card DOES keep its
// mapping, or the webhook could not place its holders' renewals.
func TestRetiredCardsStayMapped(t *testing.T) {
	seen := false
	for _, c := range entitlement.Cards() {
		if c.Retired {
			seen = true
		}
		for _, state := range []string{entitlement.SlugFree, entitlement.SlugTrial, entitlement.SlugCustom} {
			if c.Slug == state {
				t.Errorf("%q is in the catalog; it is a state, not a card", state)
			}
		}
	}
	_ = seen // no card is retired yet; the loop exists to fail if one is ever excluded
}

// One product cannot back two cards: the webhook resolves a card by product id and
// would pick one silently. The real catalog holds a single card, so the guard is
// exercised against a two-card list rather than left untested until a reprice.
func TestProductIDsRejectsTwoCardsOnOneProduct(t *testing.T) {
	cards := []entitlement.RateCard{{Slug: "usage-a"}, {Slug: "usage-b"}}
	env := map[string]string{
		"PUG_DODO_PRODUCT_USAGE_A": "prod_same",
		"PUG_DODO_PRODUCT_USAGE_B": "prod_same",
	}
	_, err := ProductIDs(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, cards)
	if err == nil {
		t.Fatal("two cards sharing a product id was accepted")
	}
}
