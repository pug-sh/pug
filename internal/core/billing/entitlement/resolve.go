// Package entitlement answers what an org is entitled to: the usage-plan catalog,
// the stored row, the live subscription the subscription package writes, and the
// resolution of the three against the clock, where a live subscription outranks the
// row. It counts nothing — consumption is internal/core/usage's job — and prices
// nothing: every rate lives on the provider's product.
//
// An allowance drives a banner and never a rejected event, so a wrong row costs a
// wrong number on a page. Nothing on the ingestion path imports this package, and
// nothing here issues a ClickHouse query.
package entitlement

import (
	"time"

	"github.com/pug-sh/pug/internal/core/billing"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Status is the entitlement state, DERIVED at read time from the billing switch and
// the live subscription. A stored status would be a second source of truth that
// can disagree with the rows beside it, and keeping it honest costs a sweep job.
type Status string

const (
	StatusActive Status = "ACTIVE"
	StatusFree   Status = "FREE"
)

// AllStatuses is every status Resolve can produce, so a table-driven RPC mapping can
// assert it covers them: a status added here and missed there ships as UNSPECIFIED.
func AllStatuses() []Status { return []Status{StatusActive, StatusFree} }

// Record is the stored entitlement row. Absent for almost every org — the normal
// state, not a defect.
type Record struct {
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	// The provider product a negotiated deal is bought against: set exactly when
	// PlanSlug is custom. Operator-written, and what makes a deal purchasable.
	ProviderProductID string
	// The catalog plan a deal splits over, set exactly when PlanSlug is custom. Pinned
	// when the product is set, since the product carries one meter per tier of the plan
	// it was made against, so a reprice never moves a deal. Never operator-written.
	BasePlanSlug string

	// 0 means no override: both columns are checked > 0, so zero cannot be a
	// stored value and neither needs a pointer to stay distinguishable.
	IncludedEventsOverride int64
	RetentionDaysOverride  int64
}

// Entitlement is the resolved answer: the plan as this org holds it, with any
// negotiated overrides applied. Nothing downstream recombines a plan with patches.
type Entitlement struct {
	Slug        string
	DisplayName string
	Status      Status

	// IncludedEvents is the free allowance: events below it are never billed, and past
	// it an org with no subscription sees a banner. nil means NONE — billing switched
	// off, or a subscription resolving to a plan the catalog no longer knows. Never
	// render it as zero.
	IncludedEvents *int64
	// How far back this org's history stays queryable. nil means NO BOUND — billing
	// off, or an unresolvable plan. Never render it as zero.
	RetentionDays *int64
	// TierUpTo is the tier layout this org's usage is split by. nil when nothing
	// splits it: billing off, free (nothing bills it), or an unknown plan. Empty is a
	// plan of one unbounded tier.
	TierUpTo []int64
	// TierPlanSlug is the catalog plan TierUpTo comes from: a subscriber's own plan,
	// or a deal's base plan. Empty exactly when TierUpTo is nil. What a record of the
	// split keeps, since TiersFor answers it and "custom" names no layout.
	TierPlanSlug string

	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool

	// The live provider subscription, if any; an empty SubStatus means none. These
	// describe the MONEY: SubPeriodStart and SubPeriodEnd bound the period the
	// provider bills, not PeriodStart and PeriodEnd. ProviderSubID names the
	// subscription the rest describe, which a caller that read it separately checks
	// it is still talking about.
	SubStatus          billing.SubStatus
	SubPeriodStart     time.Time
	SubPeriodEnd       time.Time
	ProviderCustomerID string
	ProviderSubID      string
	// When the provider's grace period for the failed card ends — the banner's
	// "update your card by". Zero outside one.
	SubGracePeriodEndsAt time.Time
}

// Resolve is the whole rule set, as a pure function. sub is separate from Record
// because their writers differ.
func Resolve(orgCreateTime time.Time, rec Record, sub *Subscription, now time.Time, billingEnabled bool) Entitlement {
	// An absent row means every field is meaningless, not just the plan: without
	// this, a caller that forgot Present would still have its anchor day honoured.
	if !rec.Present {
		rec = Record{}
	}
	// Only a live subscription supplies anything. A cancelled row is kept — "when
	// did this lapse" is a question support asks — but it is not consulted here.
	if sub != nil && !sub.Status.Live() {
		sub = nil
	}

	// Resolved through the same helper the meter uses, so the window shown and the
	// window summed are the same one. Independent of every branch below: a plan
	// changes what an org may send, never when its month turns over.
	start, end := coreusage.PeriodFor(now, coreusage.AnchorDay(orgCreateTime, rec.AnchorDay))
	ent := Entitlement{PeriodStart: start, PeriodEnd: end, BillingEnabled: billingEnabled}

	// Off means a self-hosted install, which has no allowance at all: the switch fails
	// OPEN on the number, so no banner can fire even if a client forgets to check the
	// flag. Safe precisely because the number enforces nothing.
	if !billingEnabled {
		ent.Slug, ent.DisplayName, ent.Status = SlugFree, FreeDisplayName, StatusFree
		return ent
	}
	if sub != nil {
		ent.SubStatus = sub.Status
		ent.SubPeriodStart = sub.CurrentPeriodStart
		ent.SubPeriodEnd = sub.CurrentPeriodEnd
		ent.ProviderCustomerID = sub.ProviderCustomerID
		ent.ProviderSubID = sub.ProviderSubID
		ent.SubGracePeriodEndsAt = sub.GracePeriodEndsAt
	}
	resolved := resolvePlan(&ent, rec, sub)
	// Stays once past, where it answers "when did this lapse" rather than "when will
	// it".
	ent.ContractEndsAt = rec.ContractEndsAt
	if resolved {
		applyOverrides(&ent, rec, sub, now)
	}
	return ent
}

// resolvePlan fills in the plan: a live subscription's, or the current plan's
// allowance on free. The row never supplies a plan on its own — without a
// subscription nothing bills, so a comp is an override on free, not a grant.
// False is a plan the catalog does not know, which has nothing to patch.
func resolvePlan(ent *Entitlement, rec Record, sub *Subscription) bool {
	switch {
	case sub == nil:
		fromPlan(ent, SlugFree, FreeDisplayName, CurrentPlan(), StatusFree)
		// Nothing bills a free org, so nothing splits its usage.
		ent.TierUpTo, ent.TierPlanSlug = nil, ""
		return true
	case sub.PlanSlug == SlugCustom:
		// A deal is priced by its own product over the plan pinned on its row, which a
		// reprice never moves; the row supplies whatever it negotiated on top.
		if p, ok := PlanBySlug(rec.BasePlanSlug); ok {
			fromPlan(ent, SlugCustom, CustomDisplayName, p, StatusActive)
			return true
		}
		ent.Slug, ent.DisplayName, ent.Status = SlugCustom, CustomDisplayName, StatusActive
		return false
	default:
		if p, ok := PlanBySlug(sub.PlanSlug); ok {
			fromPlan(ent, p.Slug, p.DisplayName, p, StatusActive)
			return true
		}
		// The catalog dropped a slug a paying org still holds. Keep its name with no
		// allowance and no tiers: no banner can fire, and the billing meter is to
		// report the org rather than guess a split. GetEntitlement logs it.
		ent.Slug, ent.DisplayName, ent.Status = sub.PlanSlug, sub.PlanSlug, StatusActive
		return false
	}
}

func fromPlan(ent *Entitlement, slug, name string, p Plan, status Status) {
	ent.Slug, ent.DisplayName, ent.Status = slug, name, status
	ent.IncludedEvents, ent.RetentionDays = i64(p.FreeEvents), i64(p.RetentionDays)
	// Never nil for a plan: nil is "nothing splits it", and a plan with no bounds is
	// one unbounded tier.
	ent.TierUpTo, ent.TierPlanSlug = append([]int64{}, p.TierUpTo...), p.Slug
}

// contractLapsed reports a deal whose end date has passed; a zero date is
// open-ended. Consulted for free too, because a time-boxed comp is stored on free
// and nothing else would expire it.
func contractLapsed(rec Record, now time.Time) bool {
	return !rec.ContractEndsAt.IsZero() && !now.Before(rec.ContractEndsAt)
}

// ContractEndExclusive converts the last day a deal is meant to run — what an
// operator types — into the instant to store. The comparison above is half-open,
// so storing that day's midnight would lapse the terms at the start of it. The
// date is read in lastDay's own location, so a picker in any zone means the day
// it displayed. The boundary itself is UTC midnight, like every window in this
// subsystem; building it in lastDay's zone would store a different instant per
// operator.
func ContractEndExclusive(lastDay time.Time) time.Time {
	y, m, d := lastDay.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// applyOverrides patches the negotiated fields over the resolved plan, last, so
// the deal's numbers win over the catalog's. Each override is independent.
func applyOverrides(ent *Entitlement, rec Record, sub *Subscription, now time.Time) {
	liveDeal := sub != nil && sub.PlanSlug == SlugCustom
	switch {
	case !rec.Present:
		return
	// A deal's terms are held only through its own subscription: staged and not yet
	// bought, or beside a different purchase, the org is not on the deal.
	case rec.PlanSlug == SlugCustom && !liveDeal:
		return
	// The contract cannot expire the custom subscription it covers, or a live deal
	// would lose its allowance the day its agreed term passed. Others are a different
	// purchase, and a lapsed grant's numbers must not ride along on one.
	case contractLapsed(rec, now) && !liveDeal:
		return
	}
	if rec.IncludedEventsOverride > 0 {
		v := rec.IncludedEventsOverride
		ent.IncludedEvents = &v
	}
	if rec.RetentionDaysOverride > 0 {
		v := rec.RetentionDaysOverride
		ent.RetentionDays = &v
	}
	if rec.DisplayNameOverride != "" {
		ent.DisplayName = rec.DisplayNameOverride
	}
}
