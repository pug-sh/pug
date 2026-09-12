package billing

import (
	"errors"
	"fmt"
	"slices"
)

const (
	SlugFree   = "free"
	SlugTrial  = "trial"
	SlugCustom = "custom"
)

// BlockEvents is the billing unit: every card and every deal prices per block.
const BlockEvents int64 = 100_000

// RetentionYearDays is a flat 365 days, so a leap year cannot shorten a term
// somebody bought.
const RetentionYearDays = 365

// RetentionDays is the one retention term: rendered, never enforced.
const RetentionDays = 5 * RetentionYearDays

const TrialDays = 14

const MaxTrialDays = 365

// RateCard prices usage in graduated tiers over cumulative blocks. Every field
// is immutable once an org holds the card; repricing mints a new slug.
type RateCard struct {
	Slug          string `json:"slug"`
	DisplayName   string `json:"display_name"`
	Currency      string `json:"currency"`
	BlockEvents   int64  `json:"block_events"`
	FreeBlocks    int64  `json:"free_blocks"`
	Tiers         []Tier `json:"tiers"`
	RetentionDays int64  `json:"retention_days"`
	Retired       bool   `json:"retired,omitempty"`
}

// Tier prices the blocks up to and including UpToBlock; 0 is unbounded and is
// required on the last tier and rejected on every other.
type Tier struct {
	UpToBlock     int64 `json:"up_to_block"`
	CentsPerBlock int64 `json:"cents_per_block"`
}

// CustomTerms is a negotiated deal: a flat fee, a flat block rate, or both. 0
// means none for the money fields; an allowance of 0 means charges start at the
// first block.
type CustomTerms struct {
	FlatFeeCents   int64 `json:"flat_fee_cents"`
	BlockRateCents int64 `json:"block_rate_cents"`
	IncludedEvents int64 `json:"included_events"`
}

// CurrentSlug is the card a new mandate pins.
const CurrentSlug = "usage-2026-09"

var catalog = []RateCard{
	{
		Slug: CurrentSlug, DisplayName: "Usage", Currency: Currency,
		BlockEvents: BlockEvents, FreeBlocks: 1,
		Tiers: []Tier{
			{UpToBlock: 10, CentsPerBlock: 500},
			{UpToBlock: 50, CentsPerBlock: 400},
			{UpToBlock: 0, CentsPerBlock: 300},
		},
		RetentionDays: RetentionDays,
	},
}

// Cards returns the catalog oldest first, retired cards included.
func Cards() []RateCard {
	out := make([]RateCard, 0, len(catalog))
	for _, c := range catalog {
		out = append(out, copyCard(c))
	}
	return out
}

func CardBySlug(slug string) (RateCard, bool) {
	for _, c := range catalog {
		if c.Slug == slug {
			return copyCard(c), true
		}
	}
	return RateCard{}, false
}

// CurrentCard is the newest card that is not retired: what an org with no
// mandate sees and what a new mandate pins. The panic is unreachable through a
// wired Service -- checkCatalog refuses an all-retired catalog at startup.
func CurrentCard() RateCard {
	for _, c := range slices.Backward(catalog) {
		if !c.Retired {
			return copyCard(c)
		}
	}
	panic("billing: every rate card is retired")
}

// retiredGrant is a retired card being handed to an org that does not already
// hold it. A holder keeps theirs; nobody new is put on a card that is withdrawn.
func retiredGrant(cur Record, card RateCard) bool {
	return card.Retired && cur.PlanSlug != card.Slug
}

func copyCard(c RateCard) RateCard {
	c.Tiers = append([]Tier(nil), c.Tiers...)
	return c
}

// isCardSlug reports whether a stored plan_slug names a card rather than a floor
// marker or the custom tier; the slug may still be one the catalog dropped.
func isCardSlug(slug string) bool {
	return slug != "" && slug != SlugFree && slug != SlugTrial && slug != SlugCustom
}

// checkCatalog runs at wiring time so a malformed card fails startup, not an
// invoice.
func checkCatalog() error {
	if len(catalog) == 0 {
		return errors.New("billing: the catalog has no rate card")
	}
	seen := map[string]bool{}
	live := false
	for _, c := range catalog {
		if seen[c.Slug] {
			return fmt.Errorf("billing: rate card %q is declared twice", c.Slug)
		}
		seen[c.Slug] = true
		if !isCardSlug(c.Slug) {
			return fmt.Errorf("billing: %q is not a valid rate card slug", c.Slug)
		}
		if c.BlockEvents != BlockEvents || c.FreeBlocks < 0 || c.Currency != Currency || len(c.Tiers) == 0 {
			return fmt.Errorf("billing: rate card %q is malformed", c.Slug)
		}
		prev := c.FreeBlocks
		for i, t := range c.Tiers {
			last := i == len(c.Tiers)-1
			if t.CentsPerBlock < 0 || (t.UpToBlock == 0) != last || (!last && t.UpToBlock <= prev) {
				return fmt.Errorf("billing: rate card %q tier %d is malformed", c.Slug, i)
			}
			prev = t.UpToBlock
		}
		if !c.Retired {
			live = true
		}
	}
	if !live {
		return errors.New("billing: every rate card is retired")
	}
	return nil
}
