package entitlement

import (
	"fmt"
	"slices"

	"github.com/pug-sh/pug/internal/core/billing"
)

const (
	// SlugFree and SlugCustom name STATES, not cards: free is what the current
	// card's allowance gives an org that pinned nothing, and custom is a deal whose
	// numbers live on the org's row.
	SlugFree   = "free"
	SlugTrial  = "trial"
	SlugCustom = "custom"
)

// RetentionYearDays is a flat 365 days, so a leap year cannot shorten a term
// somebody bought.
const RetentionYearDays = 365

// TrialDays is measured from orgs.create_time — the trial is the org's age, not
// stored state, so nothing is written at signup.
const TrialDays = 14

// MaxTrialDays caps one extend-trial. Past this the operator wants a deal, which
// has terms and a record.
const MaxTrialDays = 365

// CardRetentionDays is the one retention term every card promises: rendered,
// never enforced. A deal that negotiated more overrides it on its own row.
const CardRetentionDays = 5 * RetentionYearDays

type Tier struct {
	// UpToEvents is inclusive, and 0 is unbounded: required on the last tier,
	// refused on the rest.
	UpToEvents      int64
	CentsPerMillion int64
}

type RateCard struct {
	Slug        string
	DisplayName string
	Currency    string
	// FreeEvents is the allowance before charges begin, not a quota: going past it
	// costs money, it does not stop anything.
	FreeEvents    int64
	Tiers         []Tier
	RetentionDays int64
	// A retired card still resolves for existing holders but is never granted to a
	// new org, so repricing cannot go on handing out the superseded numbers.
	Retired bool
}

// CustomTerms is a negotiated deal, where a zero field means the deal has none.
// A flat fee with no rate is a fixed-price arrangement, which is how a pre-usage
// plan is expressed.
type CustomTerms struct {
	FlatFeeCents        int64
	RateCentsPerMillion int64
	IncludedEvents      int64
}

// prices reports whether these terms charge for anything. Terms with neither a
// fee nor a rate bill nothing however much is sent, so they cannot stand as what
// prices an org; migration 021's custom_needs_price refuses to store a row like
// that, and Resolve falls back to the current card if one reaches it anyway.
func (t *CustomTerms) prices() bool {
	return t != nil && (t.FlatFeeCents > 0 || t.RateCentsPerMillion > 0)
}

// rateCards is every card pug has sold, newest last: repricing mints a new slug
// and retires the old, so a holder keeps the numbers they bought.
var rateCards = []RateCard{
	{
		Slug: "usage-2026-09-1", DisplayName: "Usage", Currency: billing.Currency,
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

// CurrentCard's panic is unreachable through a wired Service: NewService refuses
// a catalog without exactly one live card.
func CurrentCard() RateCard {
	for _, c := range slices.Backward(rateCards) {
		if !c.Retired {
			return copyCard(c)
		}
	}
	panic("billing: every rate card is retired")
}

// copyCard clones the tiers too: without it a caller could reprice the catalog by
// mutating what it was handed.
func copyCard(c RateCard) RateCard {
	c.Tiers = slices.Clone(c.Tiers)
	return c
}

// isCardSlug reports whether a stored plan_slug names a card rather than a deal
// or the resolved free state; the slug may still be one the catalog dropped.
func isCardSlug(slug string) bool {
	return slug != "" && slug != SlugFree && slug != SlugCustom
}

// checkRateCards runs at wiring time so a malformed catalog fails startup rather
// than mis-pricing one request.
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
		if c.Currency != billing.Currency || c.FreeEvents < 0 || c.RetentionDays <= 0 || len(c.Tiers) == 0 {
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

func i64(v int64) *int64 { return &v }
