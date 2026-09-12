// Package billing answers what an org is entitled to send and what it owes for
// what it sent. It counts nothing: consumption is internal/core/usage's job.
//
// Nothing on the ingestion path imports this package, and nothing here issues a
// ClickHouse query. A quota drives a banner and never a rejected event.
package billing

import (
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Status is derived at read time from the timestamps, the mandate and the
// invoice ledger. A stored status would be a second source of truth.
type Status string

const (
	StatusTrialing Status = "TRIALING"
	StatusActive   Status = "ACTIVE"
	StatusFree     Status = "FREE"
	// StatusPastDue is any failed or uncollectible invoice on the ledger. Layered
	// on by the service, since Resolve does not read the ledger.
	StatusPastDue Status = "PAST_DUE"
)

func AllStatuses() []Status {
	return []Status{StatusTrialing, StatusActive, StatusFree, StatusPastDue}
}

// Record is the stored entitlement row. Absent for almost every org.
type Record struct {
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	TrialEndsAt         time.Time

	// 0 means no override.
	IncludedEventsOverride int64
	RetentionDaysOverride  int64
	FlatFeeCents           int64
	BlockRateCents         int64

	// CreateTime is when the row was first written.
	CreateTime time.Time
	// TermsEffectiveAt is when the deal's money was last changed. A deal is
	// invoiced from then, not from the row's birth: the row often predates it.
	TermsEffectiveAt time.Time
}

// dealStart is the earliest day a deal can be invoiced from.
func (r Record) dealStart() time.Time {
	if !r.TermsEffectiveAt.IsZero() {
		return r.TermsEffectiveAt
	}
	return r.CreateTime
}

// Terms is the negotiated deal on the row, if the row is one.
func (r Record) Terms() (CustomTerms, bool) {
	if !r.Present || r.PlanSlug != SlugCustom || (r.FlatFeeCents <= 0 && r.BlockRateCents <= 0) {
		return CustomTerms{}, false
	}
	return CustomTerms{
		FlatFeeCents:   r.FlatFeeCents,
		BlockRateCents: r.BlockRateCents,
		IncludedEvents: r.IncludedEventsOverride,
	}, true
}

// Entitlement is the resolved answer: the card or deal as this org holds it.
type Entitlement struct {
	Slug        string
	DisplayName string
	Currency    string
	Status      Status

	// Set only with billing off, where it is the free floor's 0. Every card and
	// deal has no list price.
	PriceCents *int64
	// Events this period before charges begin. nil means NO QUOTA: billing off, a
	// slug the catalog dropped, or a flat-fee deal. Never render it as zero.
	IncludedEvents *int64
	// nil means NO BOUND. Never render it as zero.
	RetentionDays *int64

	TrialEndsAt    time.Time
	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool

	SubStatus          SubStatus
	SubPeriodEnd       time.Time
	ProviderCustomerID string

	// Exactly one of Card and Terms is set while billing is on and the slug
	// resolves; the pricing function reads whichever it is.
	Card  *RateCard
	Terms *CustomTerms
	// Chargeable is a live on-demand mandate.
	Chargeable bool
	// NextChargeAt is PeriodEnd plus the invoicing grace; set by the service.
	NextChargeAt time.Time
}

func (e Entitlement) Pricing() Pricing { return Pricing{Card: e.Card, Terms: e.Terms} }

// Resolve is the whole rule set as a pure function over the org's age, the row,
// the mandate and the clock. Expiry is lazy, so there is no sweep job.
func Resolve(orgCreateTime time.Time, rec Record, sub *Subscription, now time.Time, billingEnabled bool) Entitlement {
	if !rec.Present {
		rec = Record{}
	}
	if sub != nil && !sub.Status.Live() {
		sub = nil
	}

	start, end := coreusage.PeriodFor(now, coreusage.AnchorDay(orgCreateTime, rec.AnchorDay))
	ent := Entitlement{PeriodStart: start, PeriodEnd: end, BillingEnabled: billingEnabled}

	// Off is a self-hosted install with no quota at all: the switch fails OPEN on
	// the number, so no banner can fire even if a client forgets the flag.
	if !billingEnabled {
		ent.Slug, ent.DisplayName, ent.Currency = SlugFree, "Free", Currency
		ent.PriceCents = i64(0)
		ent.Status = StatusFree
		return ent
	}

	if sub != nil {
		ent.SubStatus = sub.Status
		ent.SubPeriodEnd = sub.CurrentPeriodEnd
		ent.ProviderCustomerID = sub.ProviderCustomerID
		ent.Chargeable = sub.OnDemand
	}
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
		// A fee-only deal has no point at which charges begin.
		if terms.BlockRateCents > 0 {
			ent.IncludedEvents = i64(terms.IncludedEvents)
		}
		ent.RetentionDays = i64(RetentionDays)
	} else {
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

// resolveCard picks the card: the row's pin, else the mandate's, else the
// current card. A slug the catalog dropped keeps its name and no quota, since
// "free, 100k" would tell a paying customer they are over.
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
		ent.Slug, ent.DisplayName, ent.Currency = slug, slug, Currency
		return
	}
	if rec.Present && !lapsed && rec.IncludedEventsOverride > 0 {
		card.FreeBlocks = rec.IncludedEventsOverride / card.BlockEvents
	}
	ent.Slug, ent.DisplayName, ent.Currency = card.Slug, card.DisplayName, card.Currency
	ent.Card = &card
	ent.IncludedEvents = i64(card.FreeBlocks * card.BlockEvents)
	ent.RetentionDays = i64(card.RetentionDays)
}

// contractLapsed reports a deal whose end date has passed; a zero date is
// open-ended.
func contractLapsed(rec Record, now time.Time) bool {
	return !rec.ContractEndsAt.IsZero() && !now.Before(rec.ContractEndsAt)
}

// ContractEndExclusive converts the last day a deal runs, as an operator types
// it, into the half-open instant to store: the following UTC midnight, read in
// the day's own zone so a picker anywhere means the day it displayed.
func ContractEndExclusive(lastDay time.Time) time.Time {
	y, m, d := lastDay.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

func trialEnd(orgCreateTime time.Time, rec Record) time.Time {
	if !rec.TrialEndsAt.IsZero() {
		return rec.TrialEndsAt
	}
	return orgCreateTime.Add(TrialDays * 24 * time.Hour)
}

func i64(v int64) *int64 { return &v }
