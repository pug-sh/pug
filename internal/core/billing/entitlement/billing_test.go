package entitlement_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

var (
	// Created on the 10th, so a window with no anchor override runs 10th to 10th.
	created = time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)
	// Comfortably past the 14-day trial.
	later = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
)

func quota(t *testing.T, ent entitlement.Entitlement) int64 {
	t.Helper()
	if ent.IncludedEvents == nil {
		t.Fatalf("entitlement has no quota; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.IncludedEvents
}

// An org with no row is the ordinary case: its entire entitlement is its age.
func TestResolveDerivesTheFloorsFromOrgAge(t *testing.T) {
	inTrial := entitlement.Resolve(created, entitlement.Record{}, nil, created.AddDate(0, 0, 3), true)
	if inTrial.Status != entitlement.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", inTrial.Status)
	}
	// A trial carries no allowance of its own: an unpinned org is on the current
	// card, and the card's free allowance is what "free" means.
	if got, want := quota(t, inTrial), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("trial allowance = %d, want the card's %d", got, want)
	}
	if want := created.AddDate(0, 0, entitlement.TrialDays); !inTrial.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want %s", inTrial.TrialEndsAt, want)
	}

	// One tick past the trial, with nothing having run in between: expiry is lazy,
	// which is the whole reason this subsystem has no sweep job.
	expired := entitlement.Resolve(created, entitlement.Record{}, nil,
		created.AddDate(0, 0, entitlement.TrialDays).Add(time.Nanosecond), true)
	if expired.Status != entitlement.StatusFree {
		t.Errorf("status just past the trial = %s, want FREE", expired.Status)
	}
	if got, want := quota(t, expired), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance past the trial = %d, want the card's %d", got, want)
	}
}

// The quota window is the org's billing anniversary, not the 1st, and a plan
// never moves it — that is what keeps it equal to the window the meter sums.
func TestResolveWindowRunsFromTheAnniversary(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, nil, later, true)
	wantStart := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !ent.PeriodStart.Equal(wantStart) {
		t.Errorf("period_start = %s, want %s (the org signed up on the 10th)", ent.PeriodStart, wantStart)
	}

	// A contract end is the end of the AGREEMENT, and must not shorten the window.
	withContract := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug,
		ContractEndsAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}, nil, later, true)
	if !withContract.PeriodStart.Equal(ent.PeriodStart) || !withContract.PeriodEnd.Equal(ent.PeriodEnd) {
		t.Errorf("a contract moved the quota window to [%s, %s), want [%s, %s)",
			withContract.PeriodStart, withContract.PeriodEnd, ent.PeriodStart, ent.PeriodEnd)
	}

	// An explicit anchor overrides the signup day.
	anchored := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug, AnchorDay: 22,
	}, nil, later, true)
	if want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC); !anchored.PeriodStart.Equal(want) {
		t.Errorf("anchored period_start = %s, want %s", anchored.PeriodStart, want)
	}
}

func TestResolveGrantedPlan(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug,
	}, nil, later, true)

	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance = %d, want the card's %d", got, want)
	}
	// A card is what prices the org; the currency is what its tiers are quoted in.
	if ent.Card == nil {
		t.Error("no card prices an active org")
	}
	if ent.Currency != "USD" {
		t.Errorf("currency = %q, want USD", ent.Currency)
	}
}

// A lapsed contract falls to the free floor WITH the floor's numbers. Keeping the
// negotiated quota after the deal ended is the one bug here that costs money.
func TestResolveDropsAnExpiredContractToTheFloor(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ContractEndsAt:         later.Add(-time.Hour),
	}

	ent := entitlement.Resolve(created, rec, nil, later, true)
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status after the contract ended = %s, want FREE", ent.Status)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("quota after the contract ended = %d, want the card's %d", got, want)
	}
	// The pin survives the contract; only the overrides it gated fall away, leaving
	// the org on the card's own allowance, which is what free means now.
	if ent.DisplayName != entitlement.CurrentCard().DisplayName {
		t.Errorf("display name after the contract ended = %q, want the card's", ent.DisplayName)
	}
	// The date stays visible: it is now the answer to "when did this end".
	if !ent.ContractEndsAt.Equal(rec.ContractEndsAt) {
		t.Errorf("contract_ends_at = %s, want it preserved after expiry", ent.ContractEndsAt)
	}

	// One tick before it ends, the deal is still fully in force.
	stillLive := entitlement.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != entitlement.StatusActive || quota(t, stillLive) != 5_000_000 {
		t.Errorf("just before expiry: status=%s quota=%d, want ACTIVE 5000000",
			stillLive.Status, quota(t, stillLive))
	}
}

// An operator names the last day a deal runs; Resolve compares half-open. The
// conversion between the two is what keeps the org from losing the day it paid
// for, so it is pinned against Resolve rather than on its own.
func TestContractEndExclusiveCoversTheWholeNamedDay(t *testing.T) {
	lastDay := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug,
		ContractEndsAt: entitlement.ContractEndExclusive(lastDay),
	}

	lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := entitlement.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != entitlement.StatusActive {
		t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
	}
	nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := entitlement.Resolve(created, rec, nil, nextMidnight, true); got.Status != entitlement.StatusFree {
		t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
	}
}

// The trial is the org's age, and a row's mere existence is not a decision about
// it: recording an anchor day or a note must not cut a trial that is still
// running.
func TestResolveKeepsTheDerivedTrialWhenARowExists(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree, AnchorDay: 5,
	}, nil, created.AddDate(0, 0, 3), true)

	if ent.Status != entitlement.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING; storing an anchor day ended the trial", ent.Status)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance = %d, want the card's %d", got, want)
	}
}

// A comped quota is recorded on a floor plan, which has no grant to lapse — so
// the trial promotion, which renames the resolved slug to "trial", must not drop
// it.
func TestResolveKeepsAFloorPlansOverridesWhileTrialing(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme (comped)",
		TrialEndsAt:            later.AddDate(0, 0, 30),
	}, nil, later, true)

	if ent.Status != entitlement.StatusTrialing {
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
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}
	if got, want := quota(t, entitlement.Resolve(created, rec, nil, later, true)), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance after the pilot ended = %d, want the card's %d", got, want)
	}

	rec.ContractEndsAt = later.AddDate(0, 1, 0)
	if got := quota(t, entitlement.Resolve(created, rec, nil, later, true)); got != 5_000_000 {
		t.Errorf("quota while the pilot runs = %d, want 5000000", got)
	}
}

func TestResolveAppliesNegotiatedOverrides(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
	}, nil, later, true)

	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want the negotiated 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want the negotiated name", ent.DisplayName)
	}
	if ent.Slug != entitlement.SlugCustom {
		t.Errorf("slug = %q, want the tier the deal names", ent.Slug)
	}
}

// A deal is priced by its negotiated terms, not by a card: what the operator
// stored is what resolves, so nothing downstream has to recombine the two.
func TestResolveReportsACustomDealsTerms(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, IncludedEventsOverride: 5_000_000,
		FlatFeeCents: 40_000, RateCentsPerMillion: 300,
	}, nil, later, true)

	if ent.Card != nil {
		t.Error("a deal resolved onto a card; its own terms are what price it")
	}
	if ent.Terms == nil {
		t.Fatal("a deal resolved with no terms; nothing prices it")
	}
	want := entitlement.CustomTerms{
		FlatFeeCents: 40_000, RateCentsPerMillion: 300, IncludedEvents: 5_000_000,
	}
	if *ent.Terms != want {
		t.Errorf("terms = %+v, want %+v", *ent.Terms, want)
	}
}

// A deal that charges from the first event: a fee and a rate, no allowance.
// migration 021's custom_needs_price makes this a legal row, so Resolve has to
// honour it -- dropping it to the current card would undercharge the org and
// quietly void a signed deal.
func TestResolveHonoursADealWithNoAllowance(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom,
		FlatFeeCents: 40_000, RateCentsPerMillion: 300,
	}, nil, later, true)

	if ent.Slug != entitlement.SlugCustom {
		t.Fatalf("slug = %q, want custom: the deal prices this org", ent.Slug)
	}
	if ent.Terms == nil || ent.Terms.FlatFeeCents != 40_000 {
		t.Fatalf("terms = %+v, want the deal's fee and rate", ent.Terms)
	}
	// $400 flat + $3/M over 10M, with no allowance to subtract.
	q, ok := ent.Quote(10_000_000)
	if !ok || q.TotalCents != 40_000+3_000 {
		t.Errorf("quote = %d (ok=%v), want %d", q.TotalCents, ok, 40_000+3_000)
	}
}

// The money dies with the contract for the same reason the quota does: a lapsed
// deal that kept charging its negotiated fee is the expensive direction of the
// same bug.
func TestResolveDropsALapsedDealsTerms(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom,
		FlatFeeCents: 40_000, RateCentsPerMillion: 300,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}, nil, later, true)

	if ent.Terms != nil {
		t.Errorf("terms = %+v after the contract ended, want none", ent.Terms)
	}
	if ent.Slug != entitlement.CurrentCard().Slug || ent.Status != entitlement.StatusFree {
		t.Errorf("slug/status = %q/%s, want the current card and FREE", ent.Slug, ent.Status)
	}
}

// Each override patches only its own field, so a deal that changed the quota
// alone still shows the catalog's name and price.
func TestResolveOverridesAreIndependent(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug, IncludedEventsOverride: 2_000_000,
	}, nil, later, true)

	if got := quota(t, ent); got != 2_000_000 {
		t.Errorf("quota = %d, want the override", got)
	}
	if ent.DisplayName != entitlement.CurrentCard().DisplayName {
		t.Errorf("display name = %q, want the card's", ent.DisplayName)
	}
}

// An org nobody pinned is priced by the current card, which is what makes the
// catalog the default rather than a thing an operator has to apply.
func TestResolvePricesAnUnpinnedOrgFromTheCurrentCard(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, nil, later, true)

	if ent.Card == nil || ent.Card.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("card = %v, want the current one", ent.Card)
	}
	if _, ok := ent.Quote(1); !ok {
		t.Error("an unpinned org cannot be priced; it should be on the current card")
	}
}

// Switched off means a self-hosted install: no quota anywhere, so no banner can
// fire even if a client ignores billing_enabled.
func TestResolveWithBillingOffHasNoQuota(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug, IncludedEventsOverride: 5_000_000,
	}, nil, later, false)

	if ent.BillingEnabled {
		t.Error("billing_enabled is true with the switch off")
	}
	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d with billing off, want none", *ent.IncludedEvents)
	}
	// Same direction as the quota: a self-hosted install bounds nothing, and a
	// number here would be a retention promise nobody made.
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days with billing off, want none", *ent.RetentionDays)
	}
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status = %s with billing off, want FREE", ent.Status)
	}
	// The window is still real: usage is metered whether or not billing is on.
	if ent.PeriodStart.IsZero() || ent.PeriodEnd.IsZero() {
		t.Error("period bounds are missing with billing off")
	}
	// Nothing prices the org with the switch off, which is what Quote must report
	// rather than a total of zero.
	if ent.Card != nil || ent.Terms != nil {
		t.Errorf("card=%v terms=%v with billing off, want neither", ent.Card, ent.Terms)
	}
	if _, ok := ent.Quote(1_000_000); ok {
		t.Error("an org was priced with billing off; nothing should price it")
	}
}

// Only reachable if a slug is dropped from the catalog while rows still name it.
// Resolving to "free, 10,000" would tell a paying customer they are over their
// limit, so this fails open on the number instead.
func TestResolveUnknownPlanHasNoQuota(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9",
	}, nil, later, true)

	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d for an unknown plan, want none — never the free floor", *ent.IncludedEvents)
	}

	// A negotiated quota on the same row still applies: it is the customer's own
	// number and owes nothing to the catalog.
	withOverride := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9", IncludedEventsOverride: 750_000,
	}, nil, later, true)
	if got := quota(t, withOverride); got != 750_000 {
		t.Errorf("quota = %d, want the negotiated 750000", got)
	}
}

// An extended trial is the one thing that puts a trial date on the row.
func TestResolveStoredTrialWins(t *testing.T) {
	ends := later.AddDate(0, 0, 20)
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree, TrialEndsAt: ends,
	}, nil, later, true)

	if ent.Status != entitlement.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if !ent.TrialEndsAt.Equal(ends) {
		t.Errorf("trial_ends_at = %s, want the stored %s", ent.TrialEndsAt, ends)
	}

	// A paid plan outranks a lingering trial date, so a customer who converted
	// mid-trial cannot be demoted by a stale timestamp.
	converted := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.CurrentCard().Slug, TrialEndsAt: ends,
	}, nil, later, true)
	if converted.Status != entitlement.StatusActive {
		t.Errorf("status = %s for a paid plan with a live trial date, want ACTIVE", converted.Status)
	}
}

// A time-boxed comp is stored as a floor plan, and an operator may extend the
// trial on one. The contract must still end both: without this the deal expires
// onto the TRIAL floor's 500,000 rather than the free floor's 10,000, and renews
// there indefinitely.
func TestResolveExpiresAnExtendedTrialWithItsContract(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000,
		TrialEndsAt:            later.AddDate(0, 6, 0),
		ContractEndsAt:         later.Add(-time.Hour),
	}

	ent := entitlement.Resolve(created, rec, nil, later, true)
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status after the comp ended = %s, want FREE", ent.Status)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("quota after the comp ended = %d, want the card's %d", got, want)
	}

	// One tick before it ends, the extended trial is still running.
	stillLive := entitlement.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != entitlement.StatusTrialing {
		t.Errorf("status just before the comp ends = %s, want TRIALING", stillLive.Status)
	}
}

// An operator names a calendar day; the day must mean the one their picker
// showed, whatever zone it produced. Reading it in UTC first shifts the date for
// any zone east of UTC and lapses the deal before the named day begins.
func TestContractEndExclusiveIsZoneStable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lastDay time.Time
	}{
		{"UTC", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"east of UTC", time.Date(2026, 12, 31, 0, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))},
		{"west of UTC", time.Date(2026, 12, 31, 20, 0, 0, 0, time.FixedZone("PST", -8*3600))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := entitlement.Record{
				Present: true, PlanSlug: entitlement.CurrentCard().Slug,
				ContractEndsAt: entitlement.ContractEndExclusive(tc.lastDay),
			}
			lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
			if got := entitlement.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != entitlement.StatusActive {
				t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
			}
			nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			if got := entitlement.Resolve(created, rec, nil, nextMidnight, true); got.Status != entitlement.StatusFree {
				t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
			}
		})
	}
}

// Present is the whole row's discriminator. A caller that builds a Record by
// hand and forgets it must get the same answer as one that passes none, rather
// than have its anchor day and trial date honoured while its plan is ignored.
func TestResolveIgnoresEveryFieldOfAnAbsentRow(t *testing.T) {
	absent := entitlement.Resolve(created, entitlement.Record{}, nil, later, true)
	populated := entitlement.Resolve(created, entitlement.Record{
		AnchorDay:              22,
		PlanSlug:               entitlement.CurrentCard().Slug,
		IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride:  3_650,
		DisplayNameOverride:    "Acme Enterprise",
		TrialEndsAt:            later.AddDate(0, 1, 0),
		ContractEndsAt:         later.AddDate(1, 0, 0),
	}, nil, later, true)

	// Compared flattened: Entitlement holds pointers, so == would compare addresses.
	if flatten(populated) != flatten(absent) {
		t.Errorf("a Record with Present unset resolved to\n%s\nwant the no-row answer\n%s",
			flatten(populated), flatten(absent))
	}
}

// A slug the catalog dropped must not cancel a trial the org is still inside;
// the trial is its age, and owes nothing to the row's plan.
func TestResolveUnknownPlanKeepsALiveTrial(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9",
	}, nil, created.AddDate(0, 0, 3), true)

	if ent.Status != entitlement.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", ent.Status)
	}
	// The pin names a card the catalog dropped, so nothing prices this org and it
	// has no allowance -- not even during a trial. Resolving to the current card
	// would price a customer on terms nobody sold them.
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d, want none for a dropped card", *ent.IncludedEvents)
	}
}

// The trial date is kept once past, like the contract date, so a client can say
// when the trial ended rather than only when it will.
func TestResolveKeepsTheTrialDateAfterItPasses(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, nil, later, true)

	if ent.Status != entitlement.StatusFree {
		t.Fatalf("status = %s, want FREE", ent.Status)
	}
	if want := created.AddDate(0, 0, entitlement.TrialDays); !ent.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want it preserved at %s", ent.TrialEndsAt, want)
	}
}

// Retention is a term of the tier like its quota, and it moves with the plan:
// the ladder is what the pricing page sells.
func TestResolveReportsThePlansRetention(t *testing.T) {
	// Every card promises the same term -- retention stopped being a per-tier
	// differentiator when tiers became one usage card.
	card := entitlement.CurrentCard()
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: card.Slug,
	}, nil, later, true)
	if got := retention(t, ent); got != card.RetentionDays {
		t.Errorf("retention on the card = %d days, want %d", got, card.RetentionDays)
	}

	// A slug the catalog dropped promises nothing: it is unpriceable, and a
	// retention bound would be a promise nobody made.
	dropped := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "usage-1999-01-1",
	}, nil, later, true)
	if dropped.RetentionDays != nil {
		t.Errorf("retention on a dropped card = %d days, want none", *dropped.RetentionDays)
	}

	// No row at all is the current card's term, derived like everything else.
	if got, want := retention(t, entitlement.Resolve(created, entitlement.Record{}, nil, later, true)),
		entitlement.CurrentCard().RetentionDays; got != want {
		t.Errorf("retention with no row = %d days, want the card's %d", got, want)
	}
}

// A deal's retention is the org's own, and it expires with the deal — the same
// rule the quota follows, because both are terms of the same agreement.
func TestResolveAppliesANegotiatedRetention(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom,
		IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride:  10 * entitlement.RetentionYearDays,
	}

	if got := retention(t, entitlement.Resolve(created, rec, nil, later, true)); got != 3_650 {
		t.Errorf("retention = %d days, want the negotiated 3650", got)
	}

	// A lapsed deal falls to the free floor's year with everything else. Worth
	// pinning: this is the one transition that shortens a retention promise.
	rec.ContractEndsAt = later.Add(-time.Hour)
	if got, want := retention(t, entitlement.Resolve(created, rec, nil, later, true)), entitlement.CurrentCard().RetentionDays; got != want {
		t.Errorf("retention after the deal ended = %d days, want the card's %d", got, want)
	}
}

// A slug the catalog dropped keeps the row's own numbers rather than the floor's:
// imposing a shorter retention on a paying customer is the wrong way to fail.
func TestResolveUnknownPlanHasNoRetentionBound(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9",
	}, nil, later, true)
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days for an unknown plan, want none", *ent.RetentionDays)
	}

	withOverride := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9", RetentionDaysOverride: 900,
	}, nil, later, true)
	if got := retention(t, withOverride); got != 900 {
		t.Errorf("retention = %d days, want the negotiated 900", got)
	}
}

func retention(t *testing.T, ent entitlement.Entitlement) int64 {
	t.Helper()
	if ent.RetentionDays == nil {
		t.Fatalf("entitlement has no retention bound; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.RetentionDays
}

func flatten(e entitlement.Entitlement) string {
	return fmt.Sprintf("%s/%s/%s/%s quota=%v retention=%v trial=%s contract=%s window=[%s,%s) enabled=%v",
		e.Slug, e.DisplayName, e.Currency, e.Status, str(e.IncludedEvents),
		str(e.RetentionDays), e.TrialEndsAt, e.ContractEndsAt, e.PeriodStart, e.PeriodEnd, e.BillingEnabled)
}

// str renders a nullable number for a failure message: nil must read as absent
// rather than as zero, which is the whole point of the wrapper. Lived in
// plans_test.go until the plan catalog was replaced.
func str(v *int64) any {
	if v == nil {
		return "none"
	}
	return *v
}
