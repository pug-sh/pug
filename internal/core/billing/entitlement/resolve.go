// Package entitlement answers what an org is entitled to send: the plan catalog,
// the stored row, and the resolution of the two against the clock. It counts
// nothing — consumption is internal/core/usage's job, and the two meet only in a
// client rendering "X of Y".
//
// A quota drives a banner and never a rejected event, so a wrong row costs a wrong
// number on a page. Nothing on the ingestion path imports this package, and nothing
// here issues a ClickHouse query.
package entitlement

import (
	"errors"
	"time"

	"github.com/pug-sh/pug/internal/core/billing"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Status is the entitlement state, DERIVED at read time from the timestamps and
// the clock. A stored status would be a second source of truth that can disagree
// with the dates beside it, and keeping it honest costs a sweep job.
// Shared across the billing packages and read by the orgs, usage and billing
// handlers, so they live with the vocabulary rather than with one concern.
var (
	ErrOrgNotFound = errors.New("billing: org not found")
	// ErrPlanNotFound is a slug the catalog does not have. Distinct from
	// ErrPlanRetired, which is a slug it has but will not hand to a new org.
	ErrPlanNotFound = errors.New("billing: plan not found")
)

type Status string

const (
	StatusTrialing Status = "TRIALING"
	StatusActive   Status = "ACTIVE"
	StatusFree     Status = "FREE"
)

// AllStatuses is every status Resolve can produce, so a table-driven RPC mapping
// can assert it covers them: a status added here and missed there ships as
// UNSPECIFIED.
func AllStatuses() []Status { return []Status{StatusTrialing, StatusActive, StatusFree} }

// Record is the stored entitlement row. Absent for almost every org — that is
// the normal state, not a defect, and Resolve derives the floors from the org's
// age instead.
type Record struct {
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	// The provider product a negotiated deal is bought against; empty for every org
	// that is not one. Operator-written, and what makes a custom deal purchasable.
	ProviderProductID string
	TrialEndsAt       time.Time

	// 0 means no override: both columns are checked > 0, so zero cannot be a
	// stored value and neither needs a pointer to stay distinguishable.
	IncludedEventsOverride int64
	RetentionDaysOverride  int64
}

// Entitlement is the resolved answer: the plan as this org actually holds it,
// with any negotiated overrides already applied. Nothing downstream recombines
// a base plan with patches.
type Entitlement struct {
	Slug        string
	DisplayName string
	Currency    string
	Status      Status

	// Always nil under usage pricing: a graduated card has no single list price to
	// show. Kept so the proto, handler and operator CLI render "no price" rather
	// than a wrong one; sub-project 1b removes the field and its wire equivalent.
	PriceCents *int64
	// IncludedEvents is events this period before charges begin — a card's free
	// allowance or a deal's. nil means NO ALLOWANCE: billing off, or a slug no card
	// answers to. Never render it as zero.
	IncludedEvents *int64
	// How far back this org's history stays queryable. nil means NO BOUND — billing
	// off, an unresolvable plan, or a deal that named none. Never render it as zero.
	RetentionDays *int64

	// Exactly one of Card and Terms is set while billing is on and the slug
	// resolves; whichever it is, is what prices the period.
	Card  *RateCard
	Terms *CustomTerms

	TrialEndsAt    time.Time
	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool

	// The live provider subscription, if any; an empty SubStatus means none. These
	// describe the MONEY: SubPeriodEnd is when the provider bills, not PeriodEnd.
	SubStatus          billing.SubStatus
	SubPeriodEnd       time.Time
	ProviderCustomerID string
}

// Resolve is the whole rule set, as a pure function. Expiry is lazy: a trial that
// ended an hour ago resolves free on the next request, so there is no sweep job
// to leave one stale. sub is separate from Record because their writers differ.
func Resolve(orgCreateTime time.Time, rec Record, sub *billing.Subscription, now time.Time, billingEnabled bool) Entitlement {
	// An absent row means every field is meaningless, not just the plan: without
	// this, a caller that forgot Present would still have its anchor day and trial
	// date honoured while its plan was ignored.
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

	// Off means a self-hosted install, which has no quota at all: the switch fails
	// OPEN on the number, so no banner can fire even if a client forgets to check
	// the flag. Safe precisely because the number enforces nothing.
	if !billingEnabled {
		ent.Slug, ent.DisplayName, ent.Currency = SlugFree, "Free", billing.Currency
		// No card, so nothing prices this org and Quote reports so. PriceCents stays
		// nil like everywhere else under usage pricing: a graduated card has no single
		// list price, and 1b removes the field.
		ent.Status = StatusFree
		return ent
	}

	if sub != nil {
		ent.SubStatus = sub.Status
		ent.SubPeriodEnd = sub.CurrentPeriodEnd
		ent.ProviderCustomerID = sub.ProviderCustomerID
	}

	lapsed := contractLapsed(rec, now)
	ent.Status = resolveStatus(orgCreateTime, rec, sub, now, lapsed)
	resolveCard(&ent, rec, sub, lapsed)
	// Both dates stay once they are past, where they answer "when did this lapse"
	// rather than "when will it".
	ent.TrialEndsAt = trialEnd(orgCreateTime, rec)
	ent.ContractEndsAt = rec.ContractEndsAt

	applyOverrides(&ent, rec, sub, now)

	// A custom row that reached here with no allowance is a deal against nothing.
	// SetPlan refuses to write one; the current card is the backstop, which is also
	// what "free" means under usage pricing.
	if ent.Slug == SlugCustom && ent.IncludedEvents == nil {
		ent.Terms = nil
		card := CurrentCard()
		ent.Slug, ent.DisplayName, ent.Currency = card.Slug, card.DisplayName, card.Currency
		ent.Card = &card
		ent.IncludedEvents = i64(card.FreeEvents)
		ent.RetentionDays = i64(card.RetentionDays)
		ent.Status = StatusFree
	}
	return ent
}

// resolvePlan picks the tier and the state it is held in, in order: a granted
// plan beats a lingering trial date, so a customer who converted mid-trial can
// never be demoted by a stale timestamp.
// resolveStatus is what the org's billing state is called, independently of what
// prices it. A pin of any kind is a grant: with free and trial no longer plans,
// "they are on a paid tier" and "they pinned something" are the same question.
func resolveStatus(orgCreateTime time.Time, rec Record, sub *billing.Subscription, now time.Time, lapsed bool) Status {
	// Most specific first: somebody is paying for this one. Not gated on the
	// contract -- that date bounds an operator's grant, not a live subscription.
	if sub != nil {
		return StatusActive
	}
	// The slug must actually RESOLVE, not merely look like a card: an org pinned to
	// a card the catalog dropped is unpriceable, and reporting it active would claim
	// a paid subscription nothing can price.
	if rec.Present && !lapsed && rec.PlanSlug == SlugCustom {
		return StatusActive
	}
	if rec.Present && !lapsed && isCardSlug(rec.PlanSlug) {
		if _, known := CardBySlug(rec.PlanSlug); known {
			return StatusActive
		}
	}
	// Gated on the contract too: a time-boxed comp would otherwise outlive the deal
	// it belongs to and keep handing back a trial.
	if !lapsed && now.Before(trialEnd(orgCreateTime, rec)) {
		return StatusTrialing
	}
	return StatusFree
}

// resolveCard sets whichever of Card and Terms prices this org, or neither.
func resolveCard(ent *Entitlement, rec Record, sub *billing.Subscription, lapsed bool) {
	// Most specific first: somebody is paying for the subscription, so it outranks
	// both a deal on the row and an operator's grant. A live subscription naming a
	// card must not be overridden by a lapsed deal or by a slug the catalog dropped.
	if sub != nil && isCardSlug(sub.PlanSlug) {
		resolveCardSlug(ent, rec, sub.PlanSlug, lapsed)
		return
	}

	if rec.Present && rec.PlanSlug == SlugCustom {
		ent.Slug, ent.DisplayName, ent.Currency = SlugCustom, "Custom", billing.Currency
		// The money a deal is charged -- a flat fee and a rate -- arrives with
		// sub-project 1b, which adds the columns and the operator flags. Until then a
		// deal carries its allowance and no price, which Quote reports as zero.
		ent.Terms = &CustomTerms{IncludedEvents: rec.IncludedEventsOverride}
		// Gated on the contract: keeping a negotiated allowance after the deal ended
		// is the one mistake here that costs money. A lapsed deal falls through to the
		// backstop below, which puts the org back on the current card.
		if !lapsed && rec.IncludedEventsOverride > 0 {
			ent.IncludedEvents = i64(rec.IncludedEventsOverride)
		}
		return
	}

	slug := ""
	if rec.Present && isCardSlug(rec.PlanSlug) {
		slug = rec.PlanSlug
	}

	resolveCardSlug(ent, rec, slug, lapsed)
}

// resolveCardSlug puts the entitlement on the named card, or on the current one
// when the name is empty, or on nothing at all when the catalog dropped it.
func resolveCardSlug(ent *Entitlement, rec Record, slug string, lapsed bool) {
	var card RateCard
	if slug == "" {
		card = CurrentCard()
	} else if known, ok := CardBySlug(slug); ok {
		card = known
	} else {
		// The catalog dropped a slug rows still name. Resolving to the current card's
		// allowance would price a customer on terms nobody sold them.
		ent.Slug, ent.DisplayName, ent.Currency = slug, slug, billing.Currency
		// A negotiated allowance is the customer's own number and owes nothing to the
		// catalog, so it survives its card being dropped.
		if rec.Present && !lapsed && rec.IncludedEventsOverride > 0 {
			ent.IncludedEvents = i64(rec.IncludedEventsOverride)
		}
		return
	}

	// The override replaces the card's free allowance outright -- a comp is a card
	// pin with a bigger number, with no arithmetic in between.
	if rec.Present && !lapsed && rec.IncludedEventsOverride > 0 {
		card.FreeEvents = rec.IncludedEventsOverride
	}
	ent.Slug, ent.DisplayName, ent.Currency = card.Slug, card.DisplayName, card.Currency
	ent.Card = &card
	ent.IncludedEvents = i64(card.FreeEvents)
	ent.RetentionDays = i64(card.RetentionDays)
}

// contractLapsed reports a deal whose end date has passed; a zero date is
// open-ended. Consulted for the floors too, because a time-boxed comped grant is
// stored as a floor plan and nothing else would expire it.
func contractLapsed(rec Record, now time.Time) bool {
	return !rec.ContractEndsAt.IsZero() && !now.Before(rec.ContractEndsAt)
}

// ContractEndExclusive converts the last day a deal is meant to run — what an
// operator types — into the instant to store. The comparison above is half-open,
// so storing that day's midnight would lapse the plan at the start of it. The
// date is read in lastDay's own location, so a picker in any zone means the day
// it displayed. The boundary itself is UTC midnight, like every window in this
// subsystem; building it in lastDay's zone would store a different instant per
// operator.
func ContractEndExclusive(lastDay time.Time) time.Time {
	y, m, d := lastDay.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// trialEnd is the stored date when an operator extended the trial, otherwise the
// org's 14th day.
func trialEnd(orgCreateTime time.Time, rec Record) time.Time {
	if !rec.TrialEndsAt.IsZero() {
		return rec.TrialEndsAt
	}
	return orgCreateTime.Add(TrialDays * 24 * time.Hour)
}

// applyOverrides patches the negotiated fields over the resolved plan, last, so
// the deal's numbers win over the catalog's. Each override is independent.
func applyOverrides(ent *Entitlement, rec Record, sub *billing.Subscription, now time.Time) {
	// The contract cannot expire the custom subscription it covers, or a live deal
	// would lose its quota the day its agreed term passed. Others are a different
	// purchase, and a lapsed grant's numbers must not ride along on one.
	if !rec.Present || (contractLapsed(rec, now) && (sub == nil || sub.PlanSlug != SlugCustom)) {
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

// Quote prices events on whatever this entitlement resolves to. The single place
// the card-or-terms choice is made, so no caller can pick the wrong one. False is
// an entitlement nothing prices: billing off, or a slug no card answers to.
func (e Entitlement) Quote(events int64) (Quote, bool) {
	switch {
	case e.Terms != nil:
		return PriceCustom(*e.Terms, events), true
	case e.Card != nil:
		return Price(*e.Card, events), true
	}
	return Quote{}, false
}
