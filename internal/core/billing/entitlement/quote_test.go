package entitlement

import "testing"

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
