// Package billing answers what an org is entitled to send. It counts nothing:
// consumption is internal/core/usage's job, and the two meet only in a client
// rendering "X of Y".
//
// Nothing here is on the ingestion path. A quota drives a banner and never a
// rejected event, so a wrong row costs a wrong number on a page — which is why
// no ingestion path imports this package and this package reads no ClickHouse.
//
// See docs/architecture/billing.md.
package billing

import (
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// Status is the entitlement state, DERIVED at read time from timestamps and the
// clock — never stored. A stored status is a second source of truth that can
// disagree with the dates beside it, and keeping it honest costs a sweep job.
// Provider-reported states (PAST_DUE, CANCELLED) cannot be derived and arrive
// with checkout, along with a column to hold them.
type Status string

const (
	StatusTrialing Status = "TRIALING"
	StatusActive   Status = "ACTIVE"
	StatusFree     Status = "FREE"
)

// AllStatuses is every status Resolve can produce, so the RPC mapping can assert
// it covers them: a status added here and missed there ships as UNSPECIFIED.
func AllStatuses() []Status { return []Status{StatusTrialing, StatusActive, StatusFree} }

// Record is the stored entitlement row. Absent for almost every org — that is
// the normal state, not a defect, and Resolve derives the floors from the org's
// age instead.
type Record struct {
	Present bool

	AnchorDay           int
	ContractEndsAt      time.Time
	CurrencyOverride    string
	DisplayNameOverride string
	Note                string
	PlanSlug            string
	TrialEndsAt         time.Time

	// 0 means no override: the column is checked > 0, so zero cannot be a stored
	// quota and needs no pointer to stay distinguishable.
	IncludedEventsOverride int64
	// A pointer, unlike the quota above, because 0 IS a valid negotiated price —
	// a comped account — and would otherwise be indistinguishable from "unset".
	PriceCentsOverride *int64
}

// Entitlement is the resolved answer: the plan as this org actually holds it,
// with any negotiated overrides already applied. Nothing downstream recombines
// a base plan with patches.
type Entitlement struct {
	Slug        string
	DisplayName string
	Currency    string
	Status      Status

	// nil means no price to show, which is a custom deal with no price recorded.
	// Distinct from a zero price.
	PriceCents *int64
	// nil means NO QUOTA — billing is switched off, or the row names a plan the
	// catalog no longer knows. Never render it as zero.
	IncludedEvents *int64

	TrialEndsAt    time.Time
	ContractEndsAt time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time

	BillingEnabled bool
}

// Resolve is the whole rule set, as a pure function: no I/O, no clock of its
// own, so every branch is unit-testable without a container.
//
// Expiry is lazy by construction. A trial that ended an hour ago resolves free
// on the next request with nothing having run in between, which is why this
// subsystem has no sweep job whose failure could leave an entitlement stale.
func Resolve(orgCreateTime time.Time, rec Record, now time.Time, billingEnabled bool) Entitlement {
	// The quota window comes from the org's billing anniversary, resolved through
	// the same helper the meter uses so the window shown and the window summed are
	// the same one. It is independent of every branch below: a plan changes what
	// an org may send, never when its month turns over.
	start, end := coreusage.PeriodFor(now, coreusage.AnchorDay(orgCreateTime, rec.AnchorDay))
	ent := Entitlement{PeriodStart: start, PeriodEnd: end, BillingEnabled: billingEnabled}

	// Off means a self-hosted install, which has no quota at all: the switch fails
	// OPEN on the number, so no banner can fire even if a client forgets to check
	// the flag. Safe precisely because the number enforces nothing.
	if !billingEnabled {
		free := mustPlan(SlugFree)
		ent.Slug, ent.DisplayName, ent.Currency = free.Slug, free.DisplayName, free.Currency
		ent.Status = StatusFree
		return ent
	}

	plan, status := resolvePlan(orgCreateTime, rec, now)
	ent.Status = status
	ent.Slug, ent.DisplayName, ent.Currency = plan.Slug, plan.DisplayName, plan.Currency
	ent.PriceCents, ent.IncludedEvents = plan.PriceCents, plan.IncludedEvents
	if status == StatusTrialing {
		ent.TrialEndsAt = trialEnd(orgCreateTime, rec)
	}
	// Kept even once it is in the past, where it is the answer to "when did this
	// lapse" rather than "when will it".
	ent.ContractEndsAt = rec.ContractEndsAt

	applyOverrides(&ent, rec, now)
	return ent
}

// resolvePlan picks the tier and the state it is held in, in order: a paid plan
// beats a lingering trial date, so a customer who converted mid-trial can never
// be demoted by a stale timestamp.
func resolvePlan(orgCreateTime time.Time, rec Record, now time.Time) (Plan, Status) {
	free := mustPlan(SlugFree)
	trial := mustPlan(SlugTrial)

	if rec.Present {
		plan, known := PlanBySlug(rec.PlanSlug)
		if !known {
			// Resolving to "free, 10,000" would tell a paying customer they are over
			// their limit, so this keeps the row's own numbers. GetEntitlement logs it.
			return Plan{Slug: rec.PlanSlug, DisplayName: rec.PlanSlug, Currency: free.Currency}, StatusFree
		}
		if !plan.isFloor() && !contractLapsed(rec, now) {
			return plan, StatusActive
		}
	}

	// Derived identically whether or not a row exists: recording an anchor day or
	// a note must not end a trial that is still running.
	if now.Before(trialEnd(orgCreateTime, rec)) {
		return trial, StatusTrialing
	}
	return free, StatusFree
}

// contractLapsed reports a deal whose end date has passed; a zero date is
// open-ended. Consulted for floor plans too, because a time-boxed comped grant
// is stored as a floor plan plus overrides and nothing else would expire it.
func contractLapsed(rec Record, now time.Time) bool {
	return !rec.ContractEndsAt.IsZero() && !now.Before(rec.ContractEndsAt)
}

// ContractEndExclusive converts the last day a deal is meant to run — what an
// operator types — into the instant to store. The comparison above is half-open,
// so storing that day's midnight would lapse the plan at the start of it.
func ContractEndExclusive(lastDay time.Time) time.Time {
	y, m, d := lastDay.UTC().Date()
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
// repricing a catalog tier cannot disturb a deal built on it. Each override is
// independent.
func applyOverrides(ent *Entitlement, rec Record, now time.Time) {
	if !rec.Present || !overridesInForce(ent, rec, now) {
		return
	}
	if rec.IncludedEventsOverride > 0 {
		v := rec.IncludedEventsOverride
		ent.IncludedEvents = &v
	}
	if rec.DisplayNameOverride != "" {
		ent.DisplayName = rec.DisplayNameOverride
	}
	if rec.PriceCentsOverride != nil {
		v := *rec.PriceCentsOverride
		ent.PriceCents = &v
	}
	if rec.CurrencyOverride != "" {
		ent.Currency = rec.CurrencyOverride
	}
}

// overridesInForce reports whether the deal on the row still applies. It ends
// when its contract does, or when the granted plan it describes has lapsed —
// without that, an expired 5M deal would keep its 5M. A floor plan has no grant
// to lapse, so its overrides survive the promotion that renames the slug to
// "trial".
func overridesInForce(ent *Entitlement, rec Record, now time.Time) bool {
	if contractLapsed(rec, now) {
		return false
	}
	if ent.Slug == rec.PlanSlug {
		return true
	}
	stored, known := PlanBySlug(rec.PlanSlug)
	return known && stored.isFloor()
}
