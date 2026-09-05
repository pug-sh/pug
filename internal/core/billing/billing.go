// Package billing answers what an org is entitled to send. It counts nothing:
// consumption is internal/core/usage's job, and the two meet only in a client
// rendering "X of Y".
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
// the normal state, not a defect, and Resolve derives the floors from the org's
// age instead.
type Record struct {
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	TrialEndsAt         time.Time

	// 0 means no override: the column is checked > 0, so zero cannot be a stored
	// quota and needs no pointer to stay distinguishable.
	IncludedEventsOverride int64
}

// Entitlement is the resolved answer: the plan as this org actually holds it,
// with any negotiated overrides already applied. Nothing downstream recombines
// a base plan with patches.
type Entitlement struct {
	Slug        string
	DisplayName string
	Currency    string
	Status      Status

	// nil means NO LIST PRICE: the custom tier, whose price lives in the payments
	// provider, or a row naming a plan the catalog no longer knows. Zero is a real
	// price — the two floors.
	PriceCents *int64
	// nil means NO QUOTA: billing is switched off, or the row names a plan the
	// catalog no longer knows. Never render it as zero.
	IncludedEvents *int64

	TrialEndsAt    time.Time
	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool
}

// Resolve is the whole rule set, as a pure function.
//
// Expiry is lazy by construction. A trial that ended an hour ago resolves free
// on the next request with nothing having run in between, which is why this
// subsystem has no sweep job whose failure could leave an entitlement stale.
func Resolve(orgCreateTime time.Time, rec Record, now time.Time, billingEnabled bool) Entitlement {
	// An absent row means every field is meaningless, not just the plan: without
	// this, a caller that forgot Present would still have its anchor day and trial
	// date honoured while its plan was ignored.
	if !rec.Present {
		rec = Record{}
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
		free := mustPlan(SlugFree)
		ent.Slug, ent.DisplayName, ent.Currency = free.Slug, free.DisplayName, free.Currency
		// The free tier's price of 0, not nil: absent means a tier with no list
		// price, which a client would read as a negotiated deal.
		ent.PriceCents = free.PriceCents
		ent.Status = StatusFree
		return ent
	}

	plan, status := resolvePlan(orgCreateTime, rec, now)
	ent.Status = status
	ent.Slug, ent.DisplayName, ent.Currency = plan.Slug, plan.DisplayName, plan.Currency
	ent.PriceCents, ent.IncludedEvents = plan.PriceCents, plan.IncludedEvents
	// Both dates stay once they are past, where they answer "when did this lapse"
	// rather than "when will it".
	ent.TrialEndsAt = trialEnd(orgCreateTime, rec)
	ent.ContractEndsAt = rec.ContractEndsAt

	applyOverrides(&ent, rec, now)
	return ent
}

// resolvePlan picks the tier and the state it is held in, in order: a granted
// plan beats a lingering trial date, so a customer who converted mid-trial can
// never be demoted by a stale timestamp.
func resolvePlan(orgCreateTime time.Time, rec Record, now time.Time) (Plan, Status) {
	free := mustPlan(SlugFree)
	lapsed := contractLapsed(rec, now)
	plan, known := PlanBySlug(rec.PlanSlug)

	if rec.Present && known && !plan.isFloor() && !lapsed {
		return plan, StatusActive
	}
	// Gated on the contract too: a time-boxed comp is stored as a floor plan, so
	// an extended trial on one would otherwise outlive the deal it belongs to and
	// keep handing back the trial floor's much larger quota.
	if !lapsed && now.Before(trialEnd(orgCreateTime, rec)) {
		return mustPlan(SlugTrial), StatusTrialing
	}
	if rec.Present && !known {
		// Keeps the row's own numbers: resolving to "free, 10,000" would tell a
		// paying customer they are over their limit.
		return Plan{Slug: rec.PlanSlug, DisplayName: rec.PlanSlug, Currency: free.Currency}, StatusFree
	}
	return free, StatusFree
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
// the deal's numbers win over the catalog's. The deal ends when its contract
// does — without that, an expired 5M grant would keep its 5M. Each override is
// independent.
func applyOverrides(ent *Entitlement, rec Record, now time.Time) {
	if !rec.Present || contractLapsed(rec, now) {
		return
	}
	if rec.IncludedEventsOverride > 0 {
		v := rec.IncludedEventsOverride
		ent.IncludedEvents = &v
	}
	if rec.DisplayNameOverride != "" {
		ent.DisplayName = rec.DisplayNameOverride
	}
}
