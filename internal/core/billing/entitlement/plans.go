package entitlement

import (
	"errors"
	"fmt"
	"slices"
)

// Plan is a usage plan: what pug counts and how it splits a period's events into
// tiers — never what it charges. Every rate lives on the provider's product, one
// meter per tier (docs/architecture/payments.md §4.1). Go rather than
// rows, so a change to what a plan includes goes through review and deploy.
type Plan struct {
	Slug        string
	DisplayName string
	// FreeEvents is the monthly allowance: never reported to the provider, so never
	// billed. It sits below TierUpTo[0].
	FreeEvents int64
	// TierUpTo is each tier's exclusive upper bound, strictly increasing. The last
	// tier is unbounded and not listed, so a plan has len(TierUpTo)+1 tiers — and its
	// provider product one meter per tier.
	TierUpTo []int64
	// How far back the plan's history stays queryable. Rendered, never enforced.
	RetentionDays int64
	// A retired plan still resolves for the orgs on it but is never sold again.
	Retired bool
}

// Tiers is how many tiers the plan splits into, and so how many meters its
// provider product must attach.
func (p Plan) Tiers() int { return len(p.TierUpTo) + 1 }

// OnSale reports whether checkout may offer the plan: every plan until it retires.
func (p Plan) OnSale() bool { return !p.Retired }

const (
	// SlugFree is an org with no live subscription: the current plan's allowance, a
	// banner beyond it, and never a bill. A state, not a catalog entry.
	SlugFree = "free"
	// SlugCustom is a negotiated deal: its own provider product at its own rates,
	// split over the current plan's tiers. A state, not a catalog entry.
	SlugCustom = "custom"
	// SlugUsage is the plan on sale. Repricing mints a new slug and retires this one,
	// which keeps resolving for the orgs already on it.
	SlugUsage = "usage-2026-10"
)

// Display names of the two states, which have no catalog entry to carry one.
const (
	FreeDisplayName   = "Free"
	CustomDisplayName = "Custom"
)

// RetentionYearDays is a flat 365 days, so a leap year cannot shorten a term
// somebody bought.
const RetentionYearDays = 365

// PLACEHOLDERS: the commercial split is not decided yet. Each number is one
// constant, so setting it is a one-line change here plus the matching meters on the
// provider's product, and TestCatalogIsPinned's expectation. Once the plan is sold
// they are fixed (see catalog).
const (
	placeholderFreeEvents    = 100_000
	placeholderTier1UpTo     = 2_000_000
	placeholderTier2UpTo     = 15_000_000
	placeholderTier3UpTo     = 50_000_000
	placeholderTier4UpTo     = 100_000_000
	placeholderTier5UpTo     = 250_000_000
	placeholderRetentionDays = RetentionYearDays
)

// catalog is every usage plan pug has ever sold, newest last. Once any org holds a
// plan its FreeEvents, TierUpTo and RetentionDays are fixed: the tiers must match the
// meters on its provider product, and an edit would re-split every existing
// subscription silently. Repricing mints a new slug and retires the old.
var catalog = []Plan{
	{
		Slug:        SlugUsage,
		DisplayName: "Pay as you go",
		FreeEvents:  placeholderFreeEvents,
		TierUpTo: []int64{
			placeholderTier1UpTo, placeholderTier2UpTo, placeholderTier3UpTo,
			placeholderTier4UpTo, placeholderTier5UpTo,
		},
		RetentionDays: placeholderRetentionDays,
	},
}

// Plans returns the catalog in order, retired plans included; a sale path filters on
// OnSale.
func Plans() []Plan {
	out := make([]Plan, 0, len(catalog))
	for _, p := range catalog {
		out = append(out, copyPlan(p))
	}
	return out
}

// PlanBySlug reports false for free and custom, which are states rather than plans,
// and for a slug the catalog no longer knows.
func PlanBySlug(slug string) (Plan, bool) {
	for _, p := range catalog {
		if p.Slug == slug {
			return copyPlan(p), true
		}
	}
	return Plan{}, false
}

// CurrentPlan is the newest plan on sale: what an org with no subscription is
// measured against, and whose tiers a custom deal is split over. NewService checks
// one exists, so this cannot panic after wiring.
func CurrentPlan() Plan {
	for _, p := range slices.Backward(catalog) {
		if p.OnSale() {
			return copyPlan(p)
		}
	}
	panic("billing: the catalog has no plan on sale")
}

// TiersFor is the tier layout a subscription on slug is split by: its catalog plan's,
// or the current plan's for a custom deal. False for anything else, which nothing can
// split.
func TiersFor(slug string) ([]int64, bool) {
	if slug == SlugCustom {
		return CurrentPlan().TierUpTo, true
	}
	p, ok := PlanBySlug(slug)
	if !ok {
		return nil, false
	}
	return p.TierUpTo, true
}

// copyPlan detaches the tier slice, without which a caller writing through a
// returned plan would re-split the catalog for the whole process.
func copyPlan(p Plan) Plan {
	p.TierUpTo = slices.Clone(p.TierUpTo)
	return p
}

// validateCatalog checks what Resolve and the meter rely on, at wiring time rather
// than on a request: a plan on sale, no plan named like a state, and every plan's
// bounds strictly rising above its allowance.
func validateCatalog(plans []Plan) error {
	seen := map[string]bool{}
	onSale := false
	for _, p := range plans {
		switch {
		case p.Slug == "" || p.Slug == SlugFree || p.Slug == SlugCustom:
			return fmt.Errorf("billing: catalog plan %q uses a reserved slug", p.Slug)
		case seen[p.Slug]:
			return fmt.Errorf("billing: catalog lists %q twice", p.Slug)
		case p.FreeEvents < 0 || p.RetentionDays <= 0:
			return fmt.Errorf("billing: catalog plan %q has a negative allowance or no retention", p.Slug)
		}
		seen[p.Slug] = true
		prev := p.FreeEvents
		for _, bound := range p.TierUpTo {
			if bound <= prev {
				return fmt.Errorf("billing: catalog plan %q's tier bounds must rise above its allowance", p.Slug)
			}
			prev = bound
		}
		onSale = onSale || p.OnSale()
	}
	if !onSale {
		return errors.New("billing: the catalog has no plan on sale")
	}
	return nil
}

func i64(v int64) *int64 { return &v }
