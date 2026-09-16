package billing_test

import (
	"slices"
	"testing"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func pinnedCard(t *testing.T) corebilling.RateCard {
	t.Helper()
	card, ok := corebilling.CardBySlug("usage-2026-09-1")
	if !ok {
		t.Fatal("rate card usage-2026-09-1 is missing from the catalog")
	}
	return card
}

func TestPrice(t *testing.T) {
	card := pinnedCard(t)
	for _, tc := range []struct{ events, cents int64 }{
		{0, 0},
		{100_000, 0},
		{100_001, 0},
		{100_124, 0},
		{100_125, 1},
		{200_000, 400},
		{1_500_000, 5_600},
		{2_000_000, 7_600},
		{2_000_001, 7_600},
		{2_340_000, 8_552},
		{3_000_000, 10_400},
		{15_000_000, 44_000},
		{16_000_000, 46_000},
		{20_000_000, 54_000},
		{50_000_000, 114_000},
		{51_000_000, 115_400},
		{100_000_000, 184_000},
		{101_000_000, 185_000},
		{250_000_000, 334_000},
		{251_000_000, 334_700},
		{1_000_000_000_000, 700_159_000},
	} {
		q := corebilling.Price(card, tc.events)
		if q.TotalCents != tc.cents {
			t.Errorf("Price(%d) = %d cents, want %d", tc.events, q.TotalCents, tc.cents)
		}
		var cents, events int64
		for _, l := range q.Lines {
			cents += l.AmountCents
			events += l.Events
		}
		if cents != q.TotalCents || events != tc.events {
			t.Errorf("Price(%d): lines sum to %d cents over %d events", tc.events, cents, events)
		}
	}
}

func TestPriceLines(t *testing.T) {
	card := pinnedCard(t)
	free := corebilling.Line{Description: "events 1 to 100,000 free", Events: 100_000}
	for events, want := range map[int64][]corebilling.Line{
		100_000: {free},
		100_001: {free, {Description: "event 100,001", Events: 1, CentsPerMillion: 4_000}},
		2_340_000: {
			free,
			{Description: "events 100,001 to 2,000,000", Events: 1_900_000, CentsPerMillion: 4_000, AmountCents: 7_600},
			{Description: "events 2,000,001 to 2,340,000", Events: 340_000, CentsPerMillion: 2_800, AmountCents: 952},
		},
	} {
		if got := corebilling.Price(card, events).Lines; !slices.Equal(got, want) {
			t.Errorf("Price(%d) lines = %+v, want %+v", events, got, want)
		}
	}
}

// Rounding the exact total once would give 1 cent here.
func TestPriceRoundsEachLineOnItsOwn(t *testing.T) {
	card := corebilling.RateCard{Tiers: []corebilling.Tier{{UpToEvents: 125, CentsPerMillion: 4_000}, {CentsPerMillion: 4_000}}}
	if got := corebilling.Price(card, 250).TotalCents; got != 2 {
		t.Errorf("two half-cent lines = %d cents, want 2", got)
	}
}

func TestPriceIsMonotonic(t *testing.T) {
	card := pinnedCard(t)
	prev := int64(0)
	for events := int64(0); events <= 300_000_000; events += 250_000 {
		got := corebilling.Price(card, events).TotalCents
		if got < prev {
			t.Fatalf("Price(%d) = %d cents, less than %d cents for fewer events", events, got, prev)
		}
		prev = got
	}
}

func TestPriceWithNoFreeTier(t *testing.T) {
	card := pinnedCard(t)
	card.FreeEvents = 0
	want := []corebilling.Line{{Description: "events 1 to 2,000,000", Events: 2_000_000, CentsPerMillion: 4_000, AmountCents: 8_000}}
	if got := corebilling.Price(card, 2_000_000).Lines; !slices.Equal(got, want) {
		t.Errorf("Price with no free tier lines = %+v, want %+v", got, want)
	}
}

// A comp's allowance can pass a tier bound, so its first paid event is priced where the running total lands.
func TestPriceWithAnAllowanceOverATierBound(t *testing.T) {
	card := pinnedCard(t)
	card.FreeEvents = 5_000_000
	want := []corebilling.Line{
		{Description: "events 1 to 5,000,000 free", Events: 5_000_000},
		{Description: "events 5,000,001 to 15,000,000", Events: 10_000_000, CentsPerMillion: 2_800, AmountCents: 28_000},
		{Description: "events 15,000,001 to 20,000,000", Events: 5_000_000, CentsPerMillion: 2_000, AmountCents: 10_000},
	}
	if got := corebilling.Price(card, 20_000_000).Lines; !slices.Equal(got, want) {
		t.Errorf("Price with a 5,000,000 allowance lines = %+v, want %+v", got, want)
	}
}

func TestPriceCustom(t *testing.T) {
	acme := corebilling.CustomTerms{FlatFeeCents: 40_000, RateCentsPerMillion: 3_000, IncludedEvents: 5_000_000}
	for name, tc := range map[string]struct {
		terms  corebilling.CustomTerms
		events int64
		cents  int64
	}{
		"fee only":                      {corebilling.CustomTerms{FlatFeeCents: 40_000}, 7_200_000, 40_000},
		"rate only":                     {corebilling.CustomTerms{RateCentsPerMillion: 3_000}, 1_500_000, 4_500},
		"fee, rate and allowance":       {acme, 7_200_000, 46_600},
		"allowance larger than usage":   {acme, 4_000_000, 40_000},
		"a half cent rounds up":         {corebilling.CustomTerms{RateCentsPerMillion: 4_000}, 125, 1},
		"under a half cent rounds down": {corebilling.CustomTerms{RateCentsPerMillion: 4_000}, 124, 0},
		"nothing sent on a flat deal":   {corebilling.CustomTerms{FlatFeeCents: 40_000}, 0, 40_000},
		"a trillion events":             {corebilling.CustomTerms{RateCentsPerMillion: 10_000}, 1_000_000_000_000, 10_000_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			q := corebilling.PriceCustom(tc.terms, tc.events)
			if q.TotalCents != tc.cents {
				t.Errorf("PriceCustom = %d cents, want %d (lines %+v)", q.TotalCents, tc.cents, q.Lines)
			}
			var sum int64
			for _, l := range q.Lines {
				sum += l.AmountCents
			}
			if sum != q.TotalCents {
				t.Errorf("lines sum to %d cents, want %d", sum, q.TotalCents)
			}
		})
	}
}

func TestPriceCustomLines(t *testing.T) {
	terms := corebilling.CustomTerms{FlatFeeCents: 40_000, RateCentsPerMillion: 3_000, IncludedEvents: 5_000_000}
	fee := corebilling.Line{Description: "flat fee", AmountCents: 40_000}
	for events, want := range map[int64][]corebilling.Line{
		4_000_000: {fee, {Description: "events 1 to 4,000,000 included", Events: 4_000_000}},
		5_000_000: {fee, {Description: "events 1 to 5,000,000 included", Events: 5_000_000}},
		7_200_000: {
			fee,
			{Description: "events 1 to 5,000,000 included", Events: 5_000_000},
			{Description: "events 5,000,001 to 7,200,000", Events: 2_200_000, CentsPerMillion: 3_000, AmountCents: 6_600},
		},
	} {
		if got := corebilling.PriceCustom(terms, events).Lines; !slices.Equal(got, want) {
			t.Errorf("PriceCustom(%d) lines = %+v, want %+v", events, got, want)
		}
	}
}

// DisplayName is left out on purpose: a rename changes no price.
func TestRateCardsArePinned(t *testing.T) {
	want := map[string]corebilling.RateCard{
		"usage-2026-09-1": {
			Currency:   "USD",
			FreeEvents: 100_000,
			Tiers: []corebilling.Tier{
				{UpToEvents: 2_000_000, CentsPerMillion: 4_000},
				{UpToEvents: 15_000_000, CentsPerMillion: 2_800},
				{UpToEvents: 50_000_000, CentsPerMillion: 2_000},
				{UpToEvents: 100_000_000, CentsPerMillion: 1_400},
				{UpToEvents: 250_000_000, CentsPerMillion: 1_000},
				{UpToEvents: 0, CentsPerMillion: 700},
			},
			RetentionDays: 1_825,
		},
	}
	cards := corebilling.Cards()
	if len(cards) != len(want) {
		t.Errorf("catalog has %d rate cards, want %d: a card may be added, never removed while an org holds it", len(cards), len(want))
	}
	for _, c := range cards {
		w, ok := want[c.Slug]
		if !ok {
			t.Errorf("%s: new rate card, pin it here and confirm no existing card was edited", c.Slug)
			continue
		}
		if c.Currency != w.Currency || c.FreeEvents != w.FreeEvents || c.RetentionDays != w.RetentionDays ||
			c.Retired != w.Retired || !slices.Equal(c.Tiers, w.Tiers) {
			t.Errorf("%s: card = %+v, want %+v; reprice with a new slug, or pin Retired if you retired it", c.Slug, c, w)
		}
	}
}

func TestRateCardsAreNotShared(t *testing.T) {
	for name, get := range map[string]func() corebilling.RateCard{
		"CardBySlug":  func() corebilling.RateCard { return pinnedCard(t) },
		"CurrentCard": corebilling.CurrentCard,
		"Cards":       func() corebilling.RateCard { return corebilling.Cards()[0] },
	} {
		card := get()
		card.Tiers[0].CentsPerMillion = 1
		if get().Tiers[0].CentsPerMillion == 1 {
			t.Errorf("%s: writing through a returned card repriced the catalog", name)
		}
	}
}

func TestCardBySlugReportsAnUnknownSlug(t *testing.T) {
	if _, ok := corebilling.CardBySlug("usage-2099-01-1"); ok {
		t.Error("CardBySlug resolved a slug the catalog does not have")
	}
}
