// Package billing answers what an org is entitled to send and what it is priced
// at. It counts nothing: consumption is internal/core/usage's job, and the two
// meet only in a client rendering "X of Y".
//
// A quota drives a banner and never a rejected event, so a wrong row costs a
// wrong number on a page. Nothing on the ingestion path imports this package,
// and nothing here issues a ClickHouse query.
package billing

import (
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Status is the entitlement state, DERIVED at read time from the timestamps and
// the clock. A stored status would be a second source of truth that can disagree
// with the dates beside it, and keeping it honest costs a sweep job.
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
// the normal state, not a defect, and Resolve derives the rest from the org's
// age instead.
type Record struct {
	// Present is the row existing, which is no longer the same question as its
	// plan: a row can carry a trial end or an anchor day with no pin at all.
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	TrialEndsAt         time.Time

	// 0 means no override: every one of these columns is checked, so zero cannot
	// be a stored value and none needs a pointer to stay distinguishable.
	IncludedEventsOverride int64
	RetentionDaysOverride  int64
	FlatFeeCents           int64
	RateCentsPerMillion    int64

	CreateTime time.Time
	// TermsEffectiveAt is when the deal started, and the first day it is invoiced
	// from. The row usually predates the deal — extend-trial writes one months
	// earlier — so create_time cannot date one.
	TermsEffectiveAt time.Time
}

// Terms is the negotiated deal on the row, if the row is one. A deal needs a fee
// or a rate: an allowance alone is a free tier nobody agreed to.
func (r Record) Terms() (CustomTerms, bool) {
	if !r.Present || r.PlanSlug != SlugCustom || (r.FlatFeeCents <= 0 && r.RateCentsPerMillion <= 0) {
		return CustomTerms{}, false
	}
	return CustomTerms{
		FlatFeeCents:        r.FlatFeeCents,
		RateCentsPerMillion: r.RateCentsPerMillion,
		IncludedEvents:      r.IncludedEventsOverride,
	}, true
}

// Entitlement is the resolved answer: the card or deal as this org actually
// holds it, with any negotiated overrides already applied. Nothing downstream
// recombines a base plan with patches.
type Entitlement struct {
	Slug        string
	DisplayName string
	Currency    string
	Status      Status

	// IncludedEvents is events this period before charges begin — a card's free
	// allowance or a deal's. nil means NO QUOTA: billing off, a slug no card
	// answers to, or a flat-fee deal with no rate. Never render it as zero.
	IncludedEvents *int64
	// How far back this org's history stays queryable. nil means NO BOUND. Never
	// render it as zero.
	RetentionDays *int64

	TrialEndsAt    time.Time
	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool

	// The live provider subscription, if any; an empty SubStatus means none. These
	// describe the MONEY: SubPeriodEnd is when the provider bills, not PeriodEnd.
	SubStatus          SubStatus
	SubPeriodEnd       time.Time
	ProviderCustomerID string

	// Exactly one of Card and Terms is set while billing is on and the slug
	// resolves; whichever it is, is what prices the period.
	Card  *RateCard
	Terms *CustomTerms
	// Chargeable is a live payment method pug can charge. False is an org nothing
	// can be collected from, not an org with no usage.
	Chargeable bool
}

// Resolve is the whole rule set, as a pure function. Expiry is lazy: a trial that
// ended an hour ago resolves free on the next request, so there is no sweep job
// to leave one stale. sub is separate from Record because their writers differ.
func Resolve(orgCreateTime time.Time, rec Record, sub *Subscription, now time.Time, billingEnabled bool) Entitlement {
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
		ent.Slug, ent.DisplayName, ent.Currency = SlugFree, "Free", Currency
		ent.Status = StatusFree
		return ent
	}

	if sub != nil {
		ent.SubStatus = sub.Status
		ent.SubPeriodEnd = sub.CurrentPeriodEnd
		ent.ProviderCustomerID = sub.ProviderCustomerID
		ent.Chargeable = true
	}
	// Both dates stay once they are past, where they answer "when did this lapse"
	// rather than "when will it".
	ent.TrialEndsAt = trialEnd(orgCreateTime, rec)
	ent.ContractEndsAt = rec.ContractEndsAt
	lapsed := contractLapsed(rec, now)

	switch {
	case sub != nil:
		ent.Status = StatusActive
	case !lapsed && now.Before(ent.TrialEndsAt):
		ent.Status = StatusTrialing
	default:
		ent.Status = StatusFree
	}

	if terms, ok := rec.Terms(); ok && !lapsed {
		ent.Slug, ent.DisplayName, ent.Currency = SlugCustom, "Custom", Currency
		ent.Terms = &terms
		ent.Status = StatusActive
		// A flat-fee deal with no rate has no point at which charges begin, so it
		// has no allowance to render either.
		if terms.RateCentsPerMillion > 0 {
			ent.IncludedEvents = i64(terms.IncludedEvents)
		}
		ent.RetentionDays = i64(CardRetentionDays)
	} else {
		// A lapsed deal falls to the current card, not to free: the customer still
		// has a mandate, and the card is what anyone without a deal pays.
		resolveCard(&ent, rec, sub, lapsed)
	}

	if rec.Present && !lapsed {
		if rec.RetentionDaysOverride > 0 {
			ent.RetentionDays = i64(rec.RetentionDaysOverride)
		}
		if rec.DisplayNameOverride != "" {
			ent.DisplayName = rec.DisplayNameOverride
		}
	}
	return ent
}

// resolveCard picks the card an org is priced on: the operator's pin first, then
// the one its mandate checkout pinned, then the current card. An org with no
// mandate agreed to nothing, so there is nothing to grandfather.
func resolveCard(ent *Entitlement, rec Record, sub *Subscription, lapsed bool) {
	slug := ""
	switch {
	case rec.Present && isCardSlug(rec.PlanSlug):
		slug = rec.PlanSlug
	case sub != nil && isCardSlug(sub.PlanSlug):
		slug = sub.PlanSlug
	}
	var card RateCard
	if slug == "" {
		card = CurrentCard()
	} else if known, ok := CardBySlug(slug); ok {
		card = known
	} else {
		// The catalog dropped a slug rows still name. Resolving to the current
		// card's allowance would price a customer on terms nobody sold them.
		ent.Slug, ent.DisplayName, ent.Currency = slug, slug, Currency
		// A negotiated allowance is the customer's own number and owes nothing to the
		// catalog, so it survives its card being dropped.
		if rec.Present && !lapsed && rec.IncludedEventsOverride > 0 {
			ent.IncludedEvents = i64(rec.IncludedEventsOverride)
		}
		return
	}
	// The override replaces the card's free allowance outright — a comp is a card
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
// open-ended.
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
