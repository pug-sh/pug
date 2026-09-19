package entitlement

import (
	"testing"
	"time"
)

func TestEntitlementQuotePicksCardOrTerms(t *testing.T) {
	card := CurrentCard()
	ent := Entitlement{Card: &card}
	q, ok := ent.Quote(2_000_000)
	if !ok {
		t.Fatal("Quote reported no way to price a card-backed entitlement")
	}
	if want := Price(card, 2_000_000).TotalCents; q.TotalCents != want {
		t.Errorf("TotalCents = %d, want %d: Quote must price through Price", q.TotalCents, want)
	}

	terms := CustomTerms{FlatFeeCents: 4_000}
	ent = Entitlement{Terms: &terms}
	if q, ok = ent.Quote(2_000_000); !ok || q.TotalCents != 4_000 {
		t.Errorf("Quote on terms = (%d, %v), want (4000, true)", q.TotalCents, ok)
	}

	// Neither set: billing off, or a slug no card answers to.
	if _, ok = (Entitlement{}).Quote(1); ok {
		t.Error("Quote priced an entitlement with neither a card nor terms")
	}
}

func TestResolveCarriesADealsMoney(t *testing.T) {
	created := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	rec := Record{
		Present: true, PlanSlug: SlugCustom,
		FlatFeeCents:           40_000,
		RateCentsPerMillion:    3_000,
		IncludedEventsOverride: 5_000_000,
	}
	ent := Resolve(created, rec, nil, later, true)
	if ent.Terms == nil {
		t.Fatal("a custom row resolved with no terms")
	}
	if ent.Terms.FlatFeeCents != 40_000 || ent.Terms.RateCentsPerMillion != 3_000 {
		t.Errorf("terms = %+v, want the row's money", *ent.Terms)
	}
	// 5M included, then 1M at 3000/M on top of the flat fee.
	q, ok := ent.Quote(6_000_000)
	if !ok {
		t.Fatal("a deal with money could not be priced")
	}
	if want := int64(40_000 + 3_000); q.TotalCents != want {
		t.Errorf("TotalCents = %d, want %d", q.TotalCents, want)
	}
}
