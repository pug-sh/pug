package billing_test

import (
	"testing"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func current(t *testing.T) corebilling.RateCard {
	t.Helper()
	card, ok := corebilling.CardBySlug(corebilling.CurrentSlug)
	if !ok {
		t.Fatalf("the current card %q is missing from the catalog", corebilling.CurrentSlug)
	}
	return card
}

// Every row of the design's worked examples: nearest-block rounding with ties up,
// and graduated tiers that are monotonic in the count.
func TestPrice(t *testing.T) {
	card := current(t)
	for _, tc := range []struct {
		events int64
		blocks int64
		cents  int64
	}{
		{0, 0, 0},
		{49_999, 0, 0},
		{50_000, 1, 0},
		{149_999, 1, 0},
		{150_000, 2, 500},
		{1_000_000, 10, 4_500},
		{1_050_000, 11, 4_900},
		{2_340_000, 23, 9_700},
		{2_350_000, 24, 10_100},
		{5_000_000, 50, 20_500},
		{5_050_000, 51, 20_800},
		{7_000_000, 70, 26_500},
	} {
		q := corebilling.Price(card, tc.events)
		if q.Blocks != tc.blocks || q.TotalCents != tc.cents {
			t.Errorf("Price(%d) = %d blocks, %d cents; want %d blocks, %d cents",
				tc.events, q.Blocks, q.TotalCents, tc.blocks, tc.cents)
		}
		var sum, blocks int64
		for _, l := range q.Lines {
			sum += l.AmountCents
			blocks += l.Blocks
		}
		if sum != q.TotalCents || blocks != q.Blocks {
			t.Errorf("Price(%d): lines sum to %d cents / %d blocks, want %d / %d", tc.events, sum, blocks, q.TotalCents, q.Blocks)
		}
	}
}

// Graduated, not volume: sending more never costs less.
func TestPriceIsMonotonic(t *testing.T) {
	card := current(t)
	prev := int64(0)
	for events := int64(0); events <= 10_000_000; events += 50_000 {
		if got := corebilling.Price(card, events).TotalCents; got < prev {
			t.Fatalf("Price(%d) = %d cents, less than the %d cents for fewer events", events, got, prev)
		} else {
			prev = got
		}
	}
}

// Removing the free tier is a number: a card with no free block charges from the
// first block, and every pinned org keeps its own card.
func TestPriceWithNoFreeBlock(t *testing.T) {
	card := current(t)
	card.FreeBlocks = 0
	if got := corebilling.Price(card, 100_000).TotalCents; got != 500 {
		t.Errorf("first block on a card with no free tier = %d cents, want 500", got)
	}
	if got := corebilling.Price(card, 1_000_000).TotalCents; got != 5_000 {
		t.Errorf("ten blocks with no free tier = %d cents, want 5000", got)
	}
}

func TestPriceCustom(t *testing.T) {
	for name, tc := range map[string]struct {
		terms  corebilling.CustomTerms
		events int64
		cents  int64
	}{
		"fee only":                    {corebilling.CustomTerms{FlatFeeCents: 40_000}, 7_200_000, 40_000},
		"rate only, no allowance":     {corebilling.CustomTerms{BlockRateCents: 300}, 150_000, 600},
		"rate only, under allowance":  {corebilling.CustomTerms{BlockRateCents: 300, IncludedEvents: 5_000_000}, 2_000_000, 0},
		"fee, rate and allowance":     {corebilling.CustomTerms{FlatFeeCents: 40_000, BlockRateCents: 300, IncludedEvents: 5_000_000}, 7_200_000, 46_600},
		"allowance larger than usage": {corebilling.CustomTerms{FlatFeeCents: 100, BlockRateCents: 300, IncludedEvents: 50_000_000}, 7_200_000, 100},
		"nothing sent on a rate deal": {corebilling.CustomTerms{BlockRateCents: 300}, 0, 0},
		"nothing sent on a flat deal": {corebilling.CustomTerms{FlatFeeCents: 40_000}, 0, 40_000},
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
				t.Errorf("lines sum to %d, want %d", sum, q.TotalCents)
			}
		})
	}
}

// A card's numbers are fixed once any org holds it: editing them re-negotiates
// every live agreement with a one-line diff. Repricing mints a NEW slug and
// retires the old one. Editing an existing row here is almost never the fix.
func TestCatalogIsPinned(t *testing.T) {
	want := map[string]corebilling.RateCard{
		"usage-2026-09": {
			Slug: "usage-2026-09", DisplayName: "Usage", Currency: "USD",
			BlockEvents: 100_000, FreeBlocks: 1,
			Tiers:         []corebilling.Tier{{UpToBlock: 10, CentsPerBlock: 500}, {UpToBlock: 50, CentsPerBlock: 400}, {UpToBlock: 0, CentsPerBlock: 300}},
			RetentionDays: 1_825,
		},
	}
	cards := corebilling.Cards()
	if len(cards) != len(want) {
		t.Errorf("catalog has %d cards, want %d — a card may be added, but none may be REMOVED while rows still name it", len(cards), len(want))
	}
	for _, c := range cards {
		w, ok := want[c.Slug]
		if !ok {
			t.Errorf("%s: new card — add it here, and confirm nothing edited an existing one", c.Slug)
			continue
		}
		if c.Currency != w.Currency || c.BlockEvents != w.BlockEvents || c.FreeBlocks != w.FreeBlocks ||
			c.DisplayName != w.DisplayName || c.RetentionDays != w.RetentionDays ||
			c.Retired != w.Retired || len(c.Tiers) != len(w.Tiers) {
			t.Errorf("%s: card = %+v, want %+v — reprice by minting a new slug", c.Slug, c, w)
			continue
		}
		for i := range c.Tiers {
			if c.Tiers[i] != w.Tiers[i] {
				t.Errorf("%s: tier %d = %+v, want %+v", c.Slug, i, c.Tiers[i], w.Tiers[i])
			}
		}
	}
	if got := corebilling.CurrentCard().Slug; got != corebilling.CurrentSlug {
		t.Errorf("CurrentCard = %q, want %q", got, corebilling.CurrentSlug)
	}
}

// The catalog hands out copies: a caller writing through a returned tier slice
// must not reprice the card for the whole process.
func TestCardsAreNotShared(t *testing.T) {
	card := current(t)
	card.Tiers[0].CentsPerBlock = 1
	if again := current(t); again.Tiers[0].CentsPerBlock != 500 {
		t.Errorf("mutating a returned card changed the catalog: %+v", again.Tiers)
	}
}

func TestCardBySlugReportsAnUnknownSlug(t *testing.T) {
	if _, ok := corebilling.CardBySlug("usage-2099-01"); ok {
		t.Error("CardBySlug resolved a slug the catalog does not have")
	}
}

func TestBlocksRoundsToNearestWithTiesUp(t *testing.T) {
	for events, want := range map[int64]int64{
		-1: 0, 0: 0, 49_999: 0, 50_000: 1, 100_000: 1, 149_999: 1, 150_000: 2, 249_999: 2, 250_000: 3,
	} {
		if got := corebilling.Blocks(events, corebilling.BlockEvents); got != want {
			t.Errorf("Blocks(%d) = %d, want %d", events, got, want)
		}
	}
}
