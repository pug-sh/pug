package billing_test

import (
	"testing"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// A tier's money and quota are fixed once any org holds it: editing them
// re-negotiates every live agreement on that tier with a one-line diff, applied
// retroactively on deploy and recorded nowhere. Repricing mints a NEW slug
// (growth-v2) and flips the old one to Retired, so existing customers
// keep resolving against what they agreed to.
//
// This test is the only guard against that edit, because nothing else in the
// system can tell an intended reprice from a typo. If it fails, the fix is
// almost never to update the expectation.
//
// DisplayName is deliberately absent: renaming "Growth" to "Team" changes
// nothing anybody bought.
func TestCatalogIsPinned(t *testing.T) {
	type pin struct {
		currency string
		price    *int64
		events   *int64
		retired  bool
	}
	cents := func(v int64) *int64 { return &v }

	want := map[string]pin{
		"free":    {currency: "USD", price: cents(0), events: cents(10_000)},
		"trial":   {currency: "USD", price: cents(0), events: cents(500_000)},
		"starter": {currency: "USD", price: cents(1_000), events: cents(100_000)},
		"growth":  {currency: "USD", price: cents(2_000), events: cents(500_000)},
		"scale":   {currency: "USD", price: cents(3_000), events: cents(1_000_000)},
		// No price and no quota of its own: both come from the org's row.
		"custom": {currency: "USD"},
	}

	plans := corebilling.Plans()
	if len(plans) != len(want) {
		t.Errorf("catalog has %d plans, want %d — a tier may be added, but none may be REMOVED "+
			"while rows still name it", len(plans), len(want))
	}

	for _, p := range plans {
		w, ok := want[p.Slug]
		if !ok {
			t.Errorf("%s: new tier — add it here, and confirm nothing edited an existing one", p.Slug)
			continue
		}
		if p.Currency != w.currency {
			t.Errorf("%s: currency = %q, want %q", p.Slug, p.Currency, w.currency)
		}
		if !samePtr(p.PriceCents, w.price) {
			t.Errorf("%s: price_cents = %v, want %v — reprice by minting a new slug", p.Slug, str(p.PriceCents), str(w.price))
		}
		if !samePtr(p.IncludedEvents, w.events) {
			t.Errorf("%s: included_events = %v, want %v — this changes what every existing "+
				"customer on this tier gets", p.Slug, str(p.IncludedEvents), str(w.events))
		}
		if p.Retired != w.retired {
			t.Errorf("%s: retired = %v, want %v", p.Slug, p.Retired, w.retired)
		}
	}
}

// The custom tier is unresolvable without an org-level override, which the
// database enforces. Nothing may quietly give it a default.
func TestCustomPlanCarriesNoNumbersOfItsOwn(t *testing.T) {
	plan, ok := corebilling.PlanBySlug(corebilling.SlugCustom)
	if !ok {
		t.Fatal("the custom slug is missing from the catalog")
	}
	if plan.IncludedEvents != nil {
		t.Errorf("custom has a quota of %d; a deal's quota must come from its own row", *plan.IncludedEvents)
	}
	if plan.PriceCents != nil {
		t.Errorf("custom has a price of %d; a deal's price must come from its own row", *plan.PriceCents)
	}
	if plan.Retired {
		t.Error("custom is retired; an operator must still be able to grant a negotiated deal")
	}
}

func TestPlanBySlugReportsAnUnknownSlug(t *testing.T) {
	if _, ok := corebilling.PlanBySlug("growth-v9"); ok {
		t.Error("PlanBySlug resolved a slug the catalog does not have")
	}
}

func samePtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func str(v *int64) any {
	if v == nil {
		return "none"
	}
	return *v
}

// The catalog hands out pointers, so a caller writing through one would reprice
// a tier for the whole process — a mutation TestCatalogIsPinned cannot see,
// because it is not a source edit.
func TestCatalogPointersAreNotShared(t *testing.T) {
	got, ok := corebilling.PlanBySlug("growth")
	if !ok {
		t.Fatal("growth is missing from the catalog")
	}
	want := *got.IncludedEvents
	*got.IncludedEvents = 1
	*got.PriceCents = 1

	again, _ := corebilling.PlanBySlug("growth")
	if *again.IncludedEvents != want {
		t.Errorf("quota = %d after mutating a returned copy, want %d", *again.IncludedEvents, want)
	}
	for _, p := range corebilling.Plans() {
		if p.Slug == "growth" && *p.IncludedEvents != want {
			t.Errorf("Plans() quota = %d, want %d", *p.IncludedEvents, want)
		}
	}
}
