package billing

import (
	"fmt"
	"slices"
)

const CardRetentionDays = 5 * RetentionYearDays

type RateCard struct {
	Slug          string `json:"slug"`
	DisplayName   string `json:"display_name"`
	Currency      string `json:"currency"`
	FreeEvents    int64  `json:"free_events"`
	Tiers         []Tier `json:"tiers"`
	RetentionDays int64  `json:"retention_days"`
	Retired       bool   `json:"retired"`
}

// Tier's UpToEvents is inclusive, and 0 is unbounded: required on the last tier, refused on the rest.
type Tier struct {
	UpToEvents      int64 `json:"up_to_events"`
	CentsPerMillion int64 `json:"cents_per_million"`
}

// CustomTerms is a negotiated deal, where a zero field means the deal has none.
type CustomTerms struct {
	FlatFeeCents        int64 `json:"flat_fee_cents"`
	RateCentsPerMillion int64 `json:"rate_cents_per_million"`
	IncludedEvents      int64 `json:"included_events"`
}

// rateCards is every card pug has sold, newest last: repricing mints a new slug and retires the old.
var rateCards = []RateCard{
	{
		Slug: "usage-2026-09-1", DisplayName: "Usage", Currency: Currency,
		FreeEvents: 100_000,
		Tiers: []Tier{
			{UpToEvents: 2_000_000, CentsPerMillion: 4_000},
			{UpToEvents: 15_000_000, CentsPerMillion: 2_800},
			{UpToEvents: 50_000_000, CentsPerMillion: 2_000},
			{UpToEvents: 100_000_000, CentsPerMillion: 1_400},
			{UpToEvents: 250_000_000, CentsPerMillion: 1_000},
			{UpToEvents: 0, CentsPerMillion: 700},
		},
		RetentionDays: CardRetentionDays,
	},
}

func Cards() []RateCard {
	out := make([]RateCard, 0, len(rateCards))
	for _, c := range rateCards {
		out = append(out, copyCard(c))
	}
	return out
}

func CardBySlug(slug string) (RateCard, bool) {
	for _, c := range rateCards {
		if c.Slug == slug {
			return copyCard(c), true
		}
	}
	return RateCard{}, false
}

// CurrentCard's panic is unreachable through a wired Service: NewService refuses an all-retired catalog.
func CurrentCard() RateCard {
	for _, c := range slices.Backward(rateCards) {
		if !c.Retired {
			return copyCard(c)
		}
	}
	panic("billing: every rate card is retired")
}

func copyCard(c RateCard) RateCard {
	c.Tiers = slices.Clone(c.Tiers)
	return c
}

func isCardSlug(slug string) bool {
	return slug != "" && slug != SlugFree && slug != SlugTrial && slug != SlugCustom
}

func checkRateCards() error {
	seen := make(map[string]bool, len(rateCards))
	live := 0
	for _, c := range rateCards {
		if seen[c.Slug] {
			return fmt.Errorf("billing: rate card %q is declared twice", c.Slug)
		}
		seen[c.Slug] = true
		if !isCardSlug(c.Slug) {
			return fmt.Errorf("billing: %q is not a valid rate card slug", c.Slug)
		}
		if c.Currency != Currency || c.FreeEvents < 0 || c.RetentionDays <= 0 || len(c.Tiers) == 0 {
			return fmt.Errorf("billing: rate card %q is malformed", c.Slug)
		}
		prev := c.FreeEvents
		for i, t := range c.Tiers {
			last := i == len(c.Tiers)-1
			if t.CentsPerMillion < 0 || (t.UpToEvents == 0) != last || (!last && t.UpToEvents <= prev) {
				return fmt.Errorf("billing: rate card %q tier %d is malformed", c.Slug, i)
			}
			prev = t.UpToEvents
		}
		if !c.Retired {
			live++
		}
	}
	if live != 1 {
		return fmt.Errorf("billing: the catalog has %d current rate cards, want exactly one", live)
	}
	return nil
}
