package billing_test

import (
	"fmt"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

var (
	// Created on the 10th, so a window with no anchor override runs 10th to 10th.
	created = time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)
	// Comfortably past the 14-day trial.
	later = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
)

const freeAllowance = 100_000

func quota(t *testing.T, ent corebilling.Entitlement) int64 {
	t.Helper()
	if ent.IncludedEvents == nil {
		t.Fatalf("entitlement has no quota; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.IncludedEvents
}

func deal(fee, rate, allowance int64) corebilling.Record {
	return corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugCustom,
		FlatFeeCents: fee, BlockRateCents: rate, IncludedEventsOverride: allowance,
	}
}

// An org with no row is the ordinary case: its entitlement is its age and the
// current card.
func TestResolveDerivesTheFloorsFromOrgAge(t *testing.T) {
	inTrial := corebilling.Resolve(created, corebilling.Record{}, nil, created.AddDate(0, 0, 3), true)
	if inTrial.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", inTrial.Status)
	}
	if inTrial.Slug != corebilling.CurrentSlug || inTrial.Card == nil {
		t.Errorf("slug/card = %q/%v, want the current card", inTrial.Slug, inTrial.Card)
	}
	if got := quota(t, inTrial); got != freeAllowance {
		t.Errorf("trial quota = %d, want the card's free allowance %d", got, freeAllowance)
	}
	if want := created.AddDate(0, 0, corebilling.TrialDays); !inTrial.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want %s", inTrial.TrialEndsAt, want)
	}

	// One tick past the trial, with nothing having run in between: expiry is lazy.
	expired := corebilling.Resolve(created, corebilling.Record{}, nil,
		created.AddDate(0, 0, corebilling.TrialDays).Add(time.Nanosecond), true)
	if expired.Status != corebilling.StatusFree {
		t.Errorf("status just past the trial = %s, want FREE", expired.Status)
	}
	if got := quota(t, expired); got != freeAllowance {
		t.Errorf("free quota = %d, want %d", got, freeAllowance)
	}
	if expired.Chargeable {
		t.Error("an org with no mandate is chargeable")
	}
	if expired.PriceCents != nil {
		t.Errorf("price = %d, want absent — a card has no list price", *expired.PriceCents)
	}
}

// The quota window is the org's billing anniversary, not the 1st, and a plan
// never moves it — that is what keeps it equal to the window the meter sums.
func TestResolveWindowRunsFromTheAnniversary(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, nil, later, true)
	wantStart := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !ent.PeriodStart.Equal(wantStart) {
		t.Errorf("period_start = %s, want %s (the org signed up on the 10th)", ent.PeriodStart, wantStart)
	}

	withContract := deal(40_000, 0, 0)
	withContract.ContractEndsAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	got := corebilling.Resolve(created, withContract, nil, later, true)
	if !got.PeriodStart.Equal(ent.PeriodStart) || !got.PeriodEnd.Equal(ent.PeriodEnd) {
		t.Errorf("a contract moved the quota window to [%s, %s), want [%s, %s)",
			got.PeriodStart, got.PeriodEnd, ent.PeriodStart, ent.PeriodEnd)
	}

	anchored := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree, AnchorDay: 22,
	}, nil, later, true)
	if want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC); !anchored.PeriodStart.Equal(want) {
		t.Errorf("anchored period_start = %s, want %s", anchored.PeriodStart, want)
	}
}

// A pinned card is grandfathering, not a grant: the org still resolves FREE
// with no mandate, on that card's numbers.
func TestResolvePinnedCard(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.CurrentSlug,
	}, nil, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s, want FREE", ent.Status)
	}
	if ent.Card == nil || ent.Card.Slug != corebilling.CurrentSlug {
		t.Fatalf("card = %v, want the pinned card", ent.Card)
	}
	if got := quota(t, ent); got != freeAllowance {
		t.Errorf("quota = %d, want %d", got, freeAllowance)
	}
	if ent.RetentionDays == nil || *ent.RetentionDays != corebilling.RetentionDays {
		t.Errorf("retention = %v, want the card's %d", ent.RetentionDays, corebilling.RetentionDays)
	}
}

func TestResolveCustomDeal(t *testing.T) {
	rec := deal(40_000, 300, 5_000_000)
	rec.DisplayNameOverride = "Acme Enterprise"
	ent := corebilling.Resolve(created, rec, nil, later, true)

	if ent.Status != corebilling.StatusActive {
		t.Errorf("status = %s, want ACTIVE — a deal in force is a contract", ent.Status)
	}
	if ent.Slug != corebilling.SlugCustom || ent.Terms == nil {
		t.Fatalf("slug/terms = %q/%v, want custom with terms", ent.Slug, ent.Terms)
	}
	if *ent.Terms != (corebilling.CustomTerms{FlatFeeCents: 40_000, BlockRateCents: 300, IncludedEvents: 5_000_000}) {
		t.Errorf("terms = %+v", *ent.Terms)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want the deal's allowance", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want the negotiated name", ent.DisplayName)
	}
	if ent.PriceCents != nil {
		t.Errorf("price = %d, want absent", *ent.PriceCents)
	}
	if ent.Chargeable {
		t.Error("a deal with no mandate is chargeable")
	}

	// A fee-only deal has no point at which charges begin.
	if flat := corebilling.Resolve(created, deal(40_000, 0, 0), nil, later, true); flat.IncludedEvents != nil {
		t.Errorf("fee-only quota = %d, want none", *flat.IncludedEvents)
	}
	// A rate-only deal charges from the first block: a real 0.
	if metered := corebilling.Resolve(created, deal(0, 300, 0), nil, later, true); metered.IncludedEvents == nil || *metered.IncludedEvents != 0 {
		t.Errorf("rate-only quota = %v, want 0", metered.IncludedEvents)
	}
}

// A lapsed contract falls to the CURRENT CARD, not to free: the customer is
// still a customer, and the card is what anyone without a deal pays.
func TestResolveDropsAnExpiredContractToTheCard(t *testing.T) {
	rec := deal(40_000, 300, 5_000_000)
	rec.DisplayNameOverride = "Acme Enterprise"
	rec.ContractEndsAt = later.Add(-time.Hour)

	ent := corebilling.Resolve(created, rec, nil, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status after the contract ended = %s, want FREE", ent.Status)
	}
	if ent.Slug != corebilling.CurrentSlug || ent.Terms != nil || ent.Card == nil {
		t.Errorf("slug = %q terms = %v, want the current card", ent.Slug, ent.Terms)
	}
	if got := quota(t, ent); got != freeAllowance {
		t.Errorf("quota after the contract ended = %d, want the card's %d", got, freeAllowance)
	}
	if ent.DisplayName != "Usage" {
		t.Errorf("display name after the contract ended = %q, want the card's", ent.DisplayName)
	}
	// The date stays visible: it is now the answer to "when did this end".
	if !ent.ContractEndsAt.Equal(rec.ContractEndsAt) {
		t.Errorf("contract_ends_at = %s, want it preserved after expiry", ent.ContractEndsAt)
	}

	stillLive := corebilling.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != corebilling.StatusActive || quota(t, stillLive) != 5_000_000 {
		t.Errorf("just before expiry: status=%s quota=%d, want ACTIVE 5000000",
			stillLive.Status, quota(t, stillLive))
	}
}

// An operator names the last day a deal runs; Resolve compares half-open.
func TestContractEndExclusiveCoversTheWholeNamedDay(t *testing.T) {
	rec := deal(40_000, 0, 0)
	rec.ContractEndsAt = corebilling.ContractEndExclusive(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))

	lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != corebilling.StatusActive {
		t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
	}
	nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, nil, nextMidnight, true); got.Status != corebilling.StatusFree {
		t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
	}
}

// A row's mere existence is not a decision about the trial.
func TestResolveKeepsTheDerivedTrialWhenARowExists(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree, AnchorDay: 5,
	}, nil, created.AddDate(0, 0, 3), true)
	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING; storing an anchor day ended the trial", ent.Status)
	}
	pinned := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.CurrentSlug,
	}, nil, created.AddDate(0, 0, 3), true)
	if pinned.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 with a pinned card = %s, want TRIALING; a pin is not a grant", pinned.Status)
	}
}

// A comped allowance is a whole number of blocks on the org's card, and it
// survives the trial promotion.
func TestResolveCompedAllowanceWhileTrialing(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme (comped)",
		TrialEndsAt:            later.AddDate(0, 0, 30),
	}, nil, later, true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want the comped 5000000", got)
	}
	if ent.Card == nil || ent.Card.FreeBlocks != 50 {
		t.Errorf("card free blocks = %v, want 50 — the allowance must price as free blocks", ent.Card)
	}
	if ent.DisplayName != "Acme (comped)" {
		t.Errorf("display name = %q, want the negotiated one", ent.DisplayName)
	}
}

// A time-boxed comp is a deal like any other, so its allowance ends when its
// contract does.
func TestResolveExpiresACompedAllowance(t *testing.T) {
	rec := corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}
	if got := quota(t, corebilling.Resolve(created, rec, nil, later, true)); got != freeAllowance {
		t.Errorf("quota after the pilot ended = %d, want %d", got, freeAllowance)
	}
	rec.ContractEndsAt = later.AddDate(0, 1, 0)
	if got := quota(t, corebilling.Resolve(created, rec, nil, later, true)); got != 5_000_000 {
		t.Errorf("quota while the pilot runs = %d, want 5000000", got)
	}
}

// Switched off means a self-hosted install: no quota anywhere, so no banner can
// fire even if a client ignores billing_enabled.
func TestResolveWithBillingOffHasNoQuota(t *testing.T) {
	ent := corebilling.Resolve(created, deal(40_000, 300, 5_000_000), nil, later, false)
	if ent.BillingEnabled {
		t.Error("billing_enabled is true with the switch off")
	}
	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d with billing off, want none", *ent.IncludedEvents)
	}
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days with billing off, want none", *ent.RetentionDays)
	}
	if ent.Status != corebilling.StatusFree || ent.Slug != corebilling.SlugFree {
		t.Errorf("status/slug = %s/%s with billing off, want FREE/free", ent.Status, ent.Slug)
	}
	if ent.Card != nil || ent.Terms != nil {
		t.Error("billing off resolved a card or terms")
	}
	if ent.PeriodStart.IsZero() || ent.PeriodEnd.IsZero() {
		t.Error("period bounds are missing with billing off")
	}
	if ent.PriceCents == nil || *ent.PriceCents != 0 {
		t.Errorf("price = %v with billing off, want the free floor's 0", ent.PriceCents)
	}
}

// Only reachable if a card is dropped from the catalog while rows still name it.
// Resolving to the current card's allowance would tell a paying customer they
// are over their limit, so this fails open on the number instead.
func TestResolveUnknownCardHasNoQuota(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2020-01",
	}, nil, later, true)
	if ent.IncludedEvents != nil || ent.Card != nil {
		t.Errorf("quota = %v card = %v for an unknown card, want none", ent.IncludedEvents, ent.Card)
	}
	if ent.Slug != "usage-2020-01" {
		t.Errorf("slug = %q, want the row's own", ent.Slug)
	}
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days for an unknown card, want none", *ent.RetentionDays)
	}

	// The trial is the org's age and owes nothing to the row's card.
	young := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2020-01",
	}, nil, created.AddDate(0, 0, 3), true)
	if young.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", young.Status)
	}
}

// An extended trial is the one thing that puts a trial date on the row; a deal
// in force outranks it.
func TestResolveStoredTrialWins(t *testing.T) {
	ends := later.AddDate(0, 0, 20)
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree, TrialEndsAt: ends,
	}, nil, later, true)
	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if !ent.TrialEndsAt.Equal(ends) {
		t.Errorf("trial_ends_at = %s, want the stored %s", ent.TrialEndsAt, ends)
	}

	rec := deal(40_000, 0, 0)
	rec.TrialEndsAt = ends
	if converted := corebilling.Resolve(created, rec, nil, later, true); converted.Status != corebilling.StatusActive {
		t.Errorf("status = %s for a deal with a live trial date, want ACTIVE", converted.Status)
	}
}

// A time-boxed comp with an extended trial: the contract must still end both.
func TestResolveExpiresAnExtendedTrialWithItsContract(t *testing.T) {
	rec := corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugFree,
		IncludedEventsOverride: 5_000_000,
		TrialEndsAt:            later.AddDate(0, 6, 0),
		ContractEndsAt:         later.Add(-time.Hour),
	}
	ent := corebilling.Resolve(created, rec, nil, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status after the comp ended = %s, want FREE", ent.Status)
	}
	if got := quota(t, ent); got != freeAllowance {
		t.Errorf("quota after the comp ended = %d, want %d", got, freeAllowance)
	}
	stillLive := corebilling.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != corebilling.StatusTrialing {
		t.Errorf("status just before the comp ends = %s, want TRIALING", stillLive.Status)
	}
}

// An operator names a calendar day; the day must mean the one their picker
// showed, whatever zone it produced.
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
			rec := deal(40_000, 0, 0)
			rec.ContractEndsAt = corebilling.ContractEndExclusive(tc.lastDay)
			lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
			if got := corebilling.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != corebilling.StatusActive {
				t.Errorf("status at %s = %s, want ACTIVE", lateOnTheLastDay, got.Status)
			}
			nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			if got := corebilling.Resolve(created, rec, nil, nextMidnight, true); got.Status != corebilling.StatusFree {
				t.Errorf("status at %s = %s, want FREE", nextMidnight, got.Status)
			}
		})
	}
}

// Present is the whole row's discriminator.
func TestResolveIgnoresEveryFieldOfAnAbsentRow(t *testing.T) {
	absent := corebilling.Resolve(created, corebilling.Record{}, nil, later, true)
	populated := corebilling.Resolve(created, corebilling.Record{
		AnchorDay:              22,
		PlanSlug:               corebilling.SlugCustom,
		FlatFeeCents:           40_000,
		IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride:  3_650,
		DisplayNameOverride:    "Acme Enterprise",
		TrialEndsAt:            later.AddDate(0, 1, 0),
		ContractEndsAt:         later.AddDate(1, 0, 0),
	}, nil, later, true)
	if flatten(populated) != flatten(absent) {
		t.Errorf("a Record with Present unset resolved to\n%s\nwant the no-row answer\n%s",
			flatten(populated), flatten(absent))
	}
}

func TestResolveKeepsTheTrialDateAfterItPasses(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, nil, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Fatalf("status = %s, want FREE", ent.Status)
	}
	if want := created.AddDate(0, 0, corebilling.TrialDays); !ent.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want it preserved at %s", ent.TrialEndsAt, want)
	}
}

// Retention is five years on every card and every deal; a deal may name more.
func TestResolveRetention(t *testing.T) {
	if got := retention(t, corebilling.Resolve(created, corebilling.Record{}, nil, later, true)); got != corebilling.RetentionDays {
		t.Errorf("retention with no row = %d days, want %d", got, corebilling.RetentionDays)
	}
	if got := retention(t, corebilling.Resolve(created, deal(40_000, 0, 0), nil, later, true)); got != corebilling.RetentionDays {
		t.Errorf("retention on a deal = %d days, want %d", got, corebilling.RetentionDays)
	}
	rec := deal(40_000, 0, 0)
	rec.RetentionDaysOverride = 10 * corebilling.RetentionYearDays
	if got := retention(t, corebilling.Resolve(created, rec, nil, later, true)); got != 3_650 {
		t.Errorf("retention = %d days, want the negotiated 3650", got)
	}
	rec.ContractEndsAt = later.Add(-time.Hour)
	if got := retention(t, corebilling.Resolve(created, rec, nil, later, true)); got != corebilling.RetentionDays {
		t.Errorf("retention after the deal ended = %d days, want the card's %d", got, corebilling.RetentionDays)
	}
}

// Every status Resolve produces, plus the ledger's, has a place in the list a
// wire mapping asserts against.
func TestAllStatusesIncludesPastDue(t *testing.T) {
	found := false
	for _, s := range corebilling.AllStatuses() {
		if s == corebilling.StatusPastDue {
			found = true
		}
	}
	if !found {
		t.Error("AllStatuses omits PAST_DUE")
	}
}

func retention(t *testing.T, ent corebilling.Entitlement) int64 {
	t.Helper()
	if ent.RetentionDays == nil {
		t.Fatalf("entitlement has no retention bound; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.RetentionDays
}

func str(v *int64) any {
	if v == nil {
		return "none"
	}
	return *v
}

func flatten(e corebilling.Entitlement) string {
	return fmt.Sprintf("%s/%s/%s/%s quota=%v price=%v retention=%v trial=%s contract=%s window=[%s,%s) enabled=%v card=%v terms=%v",
		e.Slug, e.DisplayName, e.Currency, e.Status, str(e.IncludedEvents), str(e.PriceCents),
		str(e.RetentionDays), e.TrialEndsAt, e.ContractEndsAt, e.PeriodStart, e.PeriodEnd, e.BillingEnabled,
		e.Card != nil, e.Terms != nil)
}
