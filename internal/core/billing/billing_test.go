package billing_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

var (
	// Created on the 10th, so every derived window below runs 10th to 10th.
	created = time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)
	// Comfortably past the 14-day trial.
	later = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
)

func quota(t *testing.T, ent corebilling.Entitlement) int64 {
	t.Helper()
	if ent.IncludedEvents == nil {
		t.Fatalf("entitlement has no quota; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.IncludedEvents
}

// An org with no row is the ordinary case: its entire entitlement is its age.
func TestResolveDerivesTheFloorsFromOrgAge(t *testing.T) {
	inTrial := corebilling.Resolve(created, corebilling.Record{}, created.AddDate(0, 0, 3), true)
	if inTrial.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", inTrial.Status)
	}
	if got := quota(t, inTrial); got != 500_000 {
		t.Errorf("trial quota = %d, want 500000", got)
	}
	if want := created.AddDate(0, 0, corebilling.TrialDays); !inTrial.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want %s", inTrial.TrialEndsAt, want)
	}

	// One tick past the trial, with nothing having run in between: expiry is lazy,
	// which is the whole reason this subsystem has no sweep job.
	expired := corebilling.Resolve(created, corebilling.Record{},
		created.AddDate(0, 0, corebilling.TrialDays).Add(time.Nanosecond), true)
	if expired.Status != corebilling.StatusFree {
		t.Errorf("status just past the trial = %s, want FREE", expired.Status)
	}
	if got := quota(t, expired); got != 10_000 {
		t.Errorf("free quota = %d, want 10000", got)
	}
}

// The quota window is the org's billing anniversary, not the 1st, and a plan
// never moves it — that is what keeps it equal to the window the meter sums.
func TestResolveWindowRunsFromTheAnniversary(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, later, true)
	wantStart := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !ent.PeriodStart.Equal(wantStart) {
		t.Errorf("period_start = %s, want %s (the org signed up on the 10th)", ent.PeriodStart, wantStart)
	}

	// A contract end is the end of the AGREEMENT, and must not shorten the window.
	withContract := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth",
		ContractEndsAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}, later, true)
	if !withContract.PeriodStart.Equal(ent.PeriodStart) || !withContract.PeriodEnd.Equal(ent.PeriodEnd) {
		t.Errorf("a contract moved the quota window to [%s, %s), want [%s, %s)",
			withContract.PeriodStart, withContract.PeriodEnd, ent.PeriodStart, ent.PeriodEnd)
	}

	// An explicit anchor overrides the signup day.
	anchored := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth", AnchorDay: 22,
	}, later, true)
	if want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC); !anchored.PeriodStart.Equal(want) {
		t.Errorf("anchored period_start = %s, want %s", anchored.PeriodStart, want)
	}
}

func TestResolveGrantedPlan(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "scale",
	}, later, true)

	if ent.Status != corebilling.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if got := quota(t, ent); got != 1_000_000 {
		t.Errorf("quota = %d, want 1000000", got)
	}
	if ent.PriceCents == nil || *ent.PriceCents != 3_000 || ent.Currency != "USD" {
		t.Errorf("price = %v %s, want 3000 USD", ent.PriceCents, ent.Currency)
	}
}

// A lapsed contract falls to the free floor WITH the floor's numbers. Keeping the
// negotiated quota after the deal ended is the one bug here that costs money.
func TestResolveDropsAnExpiredContractToTheFloor(t *testing.T) {
	rec := corebilling.Record{
		Present: true, PlanSlug: "scale",
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ContractEndsAt:         later.Add(-time.Hour),
	}

	ent := corebilling.Resolve(created, rec, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status after the contract ended = %s, want FREE", ent.Status)
	}
	if got := quota(t, ent); got != 10_000 {
		t.Errorf("quota after the contract ended = %d, want the free floor's 10000", got)
	}
	if ent.DisplayName == "Acme Enterprise" {
		t.Error("a lapsed deal is still showing its negotiated name")
	}
	// The date stays visible: it is now the answer to "when did this end".
	if !ent.ContractEndsAt.Equal(rec.ContractEndsAt) {
		t.Errorf("contract_ends_at = %s, want it preserved after expiry", ent.ContractEndsAt)
	}

	// One tick before it ends, the deal is still fully in force.
	stillLive := corebilling.Resolve(created, rec, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != corebilling.StatusActive || quota(t, stillLive) != 5_000_000 {
		t.Errorf("just before expiry: status=%s quota=%d, want ACTIVE 5000000",
			stillLive.Status, quota(t, stillLive))
	}
}

// An operator names the last day a deal runs; Resolve compares half-open. The
// conversion between the two is what keeps the org from losing the day it paid
// for, so it is pinned against Resolve rather than on its own.
func TestContractEndExclusiveCoversTheWholeNamedDay(t *testing.T) {
	lastDay := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rec := corebilling.Record{
		Present: true, PlanSlug: "scale",
		ContractEndsAt: corebilling.ContractEndExclusive(lastDay),
	}

	lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, lateOnTheLastDay, true); got.Status != corebilling.StatusActive {
		t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
	}
	nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, nextMidnight, true); got.Status != corebilling.StatusFree {
		t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
	}
}

// The trial is the org's age, and a row's mere existence is not a decision about
// it: recording an anchor day or a note must not cut a trial that is still
// running.
func TestResolveKeepsTheDerivedTrialWhenARowExists(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree, AnchorDay: 5,
	}, created.AddDate(0, 0, 3), true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING; storing an anchor day ended the trial", ent.Status)
	}
	if got := quota(t, ent); got != 500_000 {
		t.Errorf("quota = %d, want the trial's 500000", got)
	}
}

// A comped quota is recorded on a floor plan, which has no grant to lapse — so
// the trial promotion, which renames the resolved slug to "trial", must not drop
// it.
func TestResolveKeepsAFloorPlansOverridesWhileTrialing(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme (comped)",
		TrialEndsAt:            later.AddDate(0, 0, 30),
	}, later, true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want the comped 5000000; extending the trial cut it", got)
	}
	if ent.DisplayName != "Acme (comped)" {
		t.Errorf("display name = %q, want the negotiated one", ent.DisplayName)
	}
}

// A time-boxed grant on a floor plan is a deal like any other, so its overrides
// end when its contract does. Without this a comped pilot never expires.
func TestResolveExpiresAFloorPlansOverrides(t *testing.T) {
	rec := corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}
	if got := quota(t, corebilling.Resolve(created, rec, later, true)); got != 10_000 {
		t.Errorf("quota after the pilot ended = %d, want the free floor's 10000", got)
	}

	rec.ContractEndsAt = later.AddDate(0, 1, 0)
	if got := quota(t, corebilling.Resolve(created, rec, later, true)); got != 5_000_000 {
		t.Errorf("quota while the pilot runs = %d, want 5000000", got)
	}
}

func TestResolveAppliesNegotiatedOverrides(t *testing.T) {
	price := int64(40_000)
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugCustom,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		PriceCentsOverride:     &price,
		CurrencyOverride:       "INR",
	}, later, true)

	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want the negotiated 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want the negotiated name", ent.DisplayName)
	}
	if ent.PriceCents == nil || *ent.PriceCents != price || ent.Currency != "INR" {
		t.Errorf("price = %v %s, want 40000 INR", ent.PriceCents, ent.Currency)
	}
	if ent.Slug != corebilling.SlugCustom {
		t.Errorf("slug = %q, want the tier the deal names", ent.Slug)
	}
}

// Each override patches only its own field, so a deal that changed the quota
// alone still shows the catalog's name and price.
func TestResolveOverridesAreIndependent(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth", IncludedEventsOverride: 2_000_000,
	}, later, true)

	if got := quota(t, ent); got != 2_000_000 {
		t.Errorf("quota = %d, want the override", got)
	}
	if ent.DisplayName != "Growth" || ent.PriceCents == nil || *ent.PriceCents != 2_000 {
		t.Errorf("name/price = %q/%v, want the catalog's Growth/2000", ent.DisplayName, ent.PriceCents)
	}
}

// A comped deal is a real price of zero, which must survive as zero rather than
// collapsing into "no price recorded".
func TestResolveKeepsAZeroPriceOverride(t *testing.T) {
	zero := int64(0)
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth", PriceCentsOverride: &zero, CurrencyOverride: "USD",
	}, later, true)

	if ent.PriceCents == nil {
		t.Fatal("a comped deal reports no price at all; want a price of 0")
	}
	if *ent.PriceCents != 0 {
		t.Errorf("price = %d, want 0", *ent.PriceCents)
	}
}

// Switched off means a self-hosted install: no quota anywhere, so no banner can
// fire even if a client ignores billing_enabled.
func TestResolveWithBillingOffHasNoQuota(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "scale", IncludedEventsOverride: 5_000_000,
	}, later, false)

	if ent.BillingEnabled {
		t.Error("billing_enabled is true with the switch off")
	}
	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d with billing off, want none", *ent.IncludedEvents)
	}
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s with billing off, want FREE", ent.Status)
	}
	// The window is still real: usage is metered whether or not billing is on.
	if ent.PeriodStart.IsZero() || ent.PeriodEnd.IsZero() {
		t.Error("period bounds are missing with billing off")
	}
}

// Only reachable if a slug is dropped from the catalog while rows still name it.
// Resolving to "free, 10,000" would tell a paying customer they are over their
// limit, so this fails open on the number instead.
func TestResolveUnknownPlanHasNoQuota(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth-v9",
	}, later, true)

	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d for an unknown plan, want none — never the free floor", *ent.IncludedEvents)
	}

	// A negotiated quota on the same row still applies: it is the customer's own
	// number and owes nothing to the catalog.
	withOverride := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "growth-v9", IncludedEventsOverride: 750_000,
	}, later, true)
	if got := quota(t, withOverride); got != 750_000 {
		t.Errorf("quota = %d, want the negotiated 750000", got)
	}
}

// An extended trial is the one thing that puts a trial date on the row.
func TestResolveStoredTrialWins(t *testing.T) {
	ends := later.AddDate(0, 0, 20)
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree, TrialEndsAt: ends,
	}, later, true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if !ent.TrialEndsAt.Equal(ends) {
		t.Errorf("trial_ends_at = %s, want the stored %s", ent.TrialEndsAt, ends)
	}

	// A paid plan outranks a lingering trial date, so a customer who converted
	// mid-trial cannot be demoted by a stale timestamp.
	converted := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "scale", TrialEndsAt: ends,
	}, later, true)
	if converted.Status != corebilling.StatusActive {
		t.Errorf("status = %s for a paid plan with a live trial date, want ACTIVE", converted.Status)
	}
}
