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

func currentCard() corebilling.RateCard { return corebilling.CurrentCard() }

func quota(t *testing.T, ent corebilling.Entitlement) int64 {
	t.Helper()
	if ent.IncludedEvents == nil {
		t.Fatalf("entitlement has no allowance; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.IncludedEvents
}

// An org with no row is the ordinary case: its entitlement is its age and the
// card everyone without a deal is on.
func TestResolveDerivesTheTrialFromOrgAge(t *testing.T) {
	card := currentCard()

	inTrial := corebilling.Resolve(created, corebilling.Record{}, nil, created.AddDate(0, 0, 3), true)
	if inTrial.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING", inTrial.Status)
	}
	// The trial is a no-charge window, not a different allowance: it is the same
	// card either side of it.
	if got := quota(t, inTrial); got != card.FreeEvents {
		t.Errorf("trial allowance = %d, want the card's %d", got, card.FreeEvents)
	}
	if want := created.AddDate(0, 0, corebilling.TrialDays); !inTrial.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want %s", inTrial.TrialEndsAt, want)
	}

	// One tick past the trial, with nothing having run in between: expiry is lazy,
	// which is the whole reason this subsystem has no sweep job.
	expired := corebilling.Resolve(created, corebilling.Record{}, nil,
		created.AddDate(0, 0, corebilling.TrialDays).Add(time.Nanosecond), true)
	if expired.Status != corebilling.StatusFree {
		t.Errorf("status just past the trial = %s, want FREE", expired.Status)
	}
	if expired.Slug != card.Slug || expired.Card == nil {
		t.Errorf("slug past the trial = %q, want the current card %q", expired.Slug, card.Slug)
	}
	if got := quota(t, expired); got != card.FreeEvents {
		t.Errorf("allowance past the trial = %d, want the card's %d", got, card.FreeEvents)
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

	// A contract end is the end of the AGREEMENT, and must not shorten the window.
	withContract := corebilling.Resolve(created, deal(), nil, later, true)
	if !withContract.PeriodStart.Equal(ent.PeriodStart) || !withContract.PeriodEnd.Equal(ent.PeriodEnd) {
		t.Errorf("a contract moved the quota window to [%s, %s), want [%s, %s)",
			withContract.PeriodStart, withContract.PeriodEnd, ent.PeriodStart, ent.PeriodEnd)
	}

	// An explicit anchor overrides the signup day.
	anchored := corebilling.Resolve(created, corebilling.Record{Present: true, AnchorDay: 22}, nil, later, true)
	if want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC); !anchored.PeriodStart.Equal(want) {
		t.Errorf("anchored period_start = %s, want %s", anchored.PeriodStart, want)
	}
}

// deal is a negotiated row in force: a fee, a rate and an allowance.
func deal() corebilling.Record {
	return corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugCustom,
		FlatFeeCents:           40_000,
		RateCentsPerMillion:    3_000,
		IncludedEventsOverride: 5_000_000,
	}
}

func TestResolveAppliesADealsTerms(t *testing.T) {
	rec := deal()
	rec.DisplayNameOverride = "Acme Enterprise"
	ent := corebilling.Resolve(created, rec, nil, later, true)

	if ent.Status != corebilling.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.Slug != corebilling.SlugCustom || ent.Terms == nil {
		t.Fatalf("slug = %q terms = %v, want the deal", ent.Slug, ent.Terms)
	}
	if ent.Card != nil {
		t.Error("a deal resolved a rate card too; exactly one of the two is set")
	}
	if ent.Terms.FlatFeeCents != 40_000 || ent.Terms.RateCentsPerMillion != 3_000 {
		t.Errorf("terms = %+v, want the row's fee and rate", *ent.Terms)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("allowance = %d, want the negotiated 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want the negotiated name", ent.DisplayName)
	}
}

// A fee-only deal has no point at which charges begin, so it has no allowance to
// render — absent, never a zero that would read as "nothing is included".
func TestResolveFlatFeeDealHasNoAllowance(t *testing.T) {
	rec := deal()
	rec.RateCentsPerMillion, rec.IncludedEventsOverride = 0, 0
	ent := corebilling.Resolve(created, rec, nil, later, true)

	if ent.Terms == nil || ent.Terms.FlatFeeCents != 40_000 {
		t.Fatalf("terms = %v, want the flat fee", ent.Terms)
	}
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d for a fee-only deal, want none", *ent.IncludedEvents)
	}
}

// A custom slug with no money behind it is not a deal at all — the check
// constraint refuses to store one — so it resolves on the card like anyone else.
func TestResolveCustomSlugWithNoMoneyIsNotADeal(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: corebilling.SlugCustom,
	}, nil, later, true)

	if ent.Terms != nil {
		t.Errorf("terms = %+v, want none: an allowance alone is not a deal", *ent.Terms)
	}
	if ent.Slug != currentCard().Slug || ent.Card == nil {
		t.Errorf("slug = %q, want the current card", ent.Slug)
	}
}

// A lapsed deal falls to the CURRENT CARD, not to free: the deal ended, the
// customer did not, and the card is what anyone without a deal pays.
func TestResolveDropsALapsedDealToTheCurrentCard(t *testing.T) {
	card := currentCard()
	rec := deal()
	rec.DisplayNameOverride = "Acme Enterprise"
	rec.ContractEndsAt = later.Add(-time.Hour)

	ent := corebilling.Resolve(created, rec, nil, later, true)
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status after the deal ended = %s, want FREE", ent.Status)
	}
	if ent.Terms != nil {
		t.Errorf("terms = %+v after the deal ended, want none", *ent.Terms)
	}
	if ent.Slug != card.Slug || ent.Card == nil {
		t.Errorf("slug after the deal ended = %q, want the current card %q", ent.Slug, card.Slug)
	}
	if got := quota(t, ent); got != card.FreeEvents {
		t.Errorf("allowance after the deal ended = %d, want the card's %d", got, card.FreeEvents)
	}
	if ent.DisplayName != card.DisplayName {
		t.Errorf("display name after the deal ended = %q, want the card's", ent.DisplayName)
	}
	// The date stays visible: it is now the answer to "when did this end".
	if !ent.ContractEndsAt.Equal(rec.ContractEndsAt) {
		t.Errorf("contract_ends_at = %s, want it preserved after expiry", ent.ContractEndsAt)
	}

	// One tick before it ends, the deal is still fully in force.
	stillLive := corebilling.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != corebilling.StatusActive || quota(t, stillLive) != 5_000_000 {
		t.Errorf("just before expiry: status=%s allowance=%d, want ACTIVE 5000000",
			stillLive.Status, quota(t, stillLive))
	}
}

// An operator names the last day a deal runs; Resolve compares half-open. The
// conversion between the two is what keeps the org from losing the day it paid
// for, so it is pinned against Resolve rather than on its own.
func TestContractEndExclusiveCoversTheWholeNamedDay(t *testing.T) {
	lastDay := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rec := deal()
	rec.ContractEndsAt = corebilling.ContractEndExclusive(lastDay)

	lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != corebilling.StatusActive {
		t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
	}
	nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := corebilling.Resolve(created, rec, nil, nextMidnight, true); got.Status != corebilling.StatusFree {
		t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
	}
}

// The trial is the org's age, and a row's mere existence is not a decision about
// it: recording an anchor day or a note must not cut a trial that is still
// running.
func TestResolveKeepsTheDerivedTrialWhenARowExists(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, AnchorDay: 5,
	}, nil, created.AddDate(0, 0, 3), true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status on day 3 = %s, want TRIALING; storing an anchor day ended the trial", ent.Status)
	}
}

// A comp is a card pin with a bigger allowance, which replaces the card's own
// outright — there is no arithmetic in between.
func TestResolveOverrideReplacesTheCardsAllowance(t *testing.T) {
	card := currentCard()
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: card.Slug,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme (comped)",
	}, nil, later, true)

	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("allowance = %d, want the comped 5000000", got)
	}
	if ent.Card == nil || ent.Card.FreeEvents != 5_000_000 {
		t.Errorf("the card's free events = %v, want the override to replace them", ent.Card)
	}
	if ent.DisplayName != "Acme (comped)" {
		t.Errorf("display name = %q, want the negotiated one", ent.DisplayName)
	}
	// The rates are untouched: an allowance is not a reprice.
	if ent.Card.Tiers[0].CentsPerMillion != card.Tiers[0].CentsPerMillion {
		t.Error("an allowance override changed the card's rates")
	}
}

// A time-boxed comp ends with its contract. Without this a comped pilot never
// expires.
func TestResolveExpiresACompWithItsContract(t *testing.T) {
	card := currentCard()
	rec := corebilling.Record{
		Present: true, PlanSlug: card.Slug,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}
	if got := quota(t, corebilling.Resolve(created, rec, nil, later, true)); got != card.FreeEvents {
		t.Errorf("allowance after the pilot ended = %d, want the card's %d", got, card.FreeEvents)
	}

	rec.ContractEndsAt = later.AddDate(0, 1, 0)
	if got := quota(t, corebilling.Resolve(created, rec, nil, later, true)); got != 5_000_000 {
		t.Errorf("allowance while the pilot runs = %d, want 5000000", got)
	}
}

// Switched off means a self-hosted install: nothing to price and no quota
// anywhere, so no banner can fire even if a client ignores billing_enabled.
func TestResolveWithBillingOffHasNothingToPrice(t *testing.T) {
	ent := corebilling.Resolve(created, deal(), nil, later, false)

	if ent.BillingEnabled {
		t.Error("billing_enabled is true with the switch off")
	}
	if ent.Card != nil || ent.Terms != nil {
		t.Errorf("card = %v terms = %v with billing off, want neither", ent.Card, ent.Terms)
	}
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d with billing off, want none", *ent.IncludedEvents)
	}
	// Same direction as the allowance: a self-hosted install bounds nothing, and a
	// number here would be a retention promise nobody made.
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days with billing off, want none", *ent.RetentionDays)
	}
	if ent.Status != corebilling.StatusFree || ent.Slug != corebilling.SlugFree {
		t.Errorf("status/slug = %s/%q with billing off, want FREE/free", ent.Status, ent.Slug)
	}
	if ent.Chargeable {
		t.Error("chargeable with billing off")
	}
	// The window is still real: usage is metered whether or not billing is on.
	if ent.PeriodStart.IsZero() || ent.PeriodEnd.IsZero() {
		t.Error("period bounds are missing with billing off")
	}
}

// Only reachable if a card is dropped from the catalog while rows still name it.
// Pricing it on the current card would charge terms nobody sold them.
func TestResolveUnknownCardKeepsItsName(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2019-01-1",
	}, nil, later, true)

	if ent.Slug != "usage-2019-01-1" {
		t.Errorf("slug = %q, want the row's own", ent.Slug)
	}
	if ent.Card != nil {
		t.Error("an unknown slug resolved a card; there is none to resolve")
	}
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d for an unknown card, want none", *ent.IncludedEvents)
	}

	// A negotiated allowance on the same row still applies: it is the customer's
	// own number and owes nothing to the catalog.
	withOverride := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2019-01-1", IncludedEventsOverride: 750_000,
	}, nil, later, true)
	if got := quota(t, withOverride); got != 750_000 {
		t.Errorf("allowance = %d, want the negotiated 750000", got)
	}
}

// An extended trial is the one thing that puts a trial date on the row.
func TestResolveStoredTrialWins(t *testing.T) {
	ends := later.AddDate(0, 0, 20)
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, TrialEndsAt: ends,
	}, nil, later, true)

	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s, want TRIALING", ent.Status)
	}
	if !ent.TrialEndsAt.Equal(ends) {
		t.Errorf("trial_ends_at = %s, want the stored %s", ent.TrialEndsAt, ends)
	}

	// A deal outranks a lingering trial date, so a customer who converted mid-trial
	// cannot be demoted by a stale timestamp.
	rec := deal()
	rec.TrialEndsAt = ends
	if converted := corebilling.Resolve(created, rec, nil, later, true); converted.Status != corebilling.StatusActive {
		t.Errorf("status = %s for a deal with a live trial date, want ACTIVE", converted.Status)
	}
}

// A contract ends an extended trial too: without this a comped pilot's trial
// outlives the deal it belongs to and renews indefinitely.
func TestResolveExpiresAnExtendedTrialWithItsContract(t *testing.T) {
	rec := corebilling.Record{
		Present:        true,
		TrialEndsAt:    later.AddDate(0, 6, 0),
		ContractEndsAt: later.Add(-time.Hour),
	}

	if ent := corebilling.Resolve(created, rec, nil, later, true); ent.Status != corebilling.StatusFree {
		t.Errorf("status after the comp ended = %s, want FREE", ent.Status)
	}
	// One tick before it ends, the extended trial is still running.
	stillLive := corebilling.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), true)
	if stillLive.Status != corebilling.StatusTrialing {
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
			rec := deal()
			rec.ContractEndsAt = corebilling.ContractEndExclusive(tc.lastDay)
			lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
			if got := corebilling.Resolve(created, rec, nil, lateOnTheLastDay, true); got.Status != corebilling.StatusActive {
				t.Errorf("status at %s = %s, want ACTIVE — the named day is inclusive", lateOnTheLastDay, got.Status)
			}
			nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			if got := corebilling.Resolve(created, rec, nil, nextMidnight, true); got.Status != corebilling.StatusFree {
				t.Errorf("status at %s = %s, want FREE — the deal ends when the day does", nextMidnight, got.Status)
			}
		})
	}
}

// Present is the whole row's discriminator. A caller that builds a Record by
// hand and forgets it must get the same answer as one that passes none, rather
// than have its anchor day and trial date honoured while its plan is ignored.
func TestResolveIgnoresEveryFieldOfAnAbsentRow(t *testing.T) {
	absent := corebilling.Resolve(created, corebilling.Record{}, nil, later, true)
	populated := corebilling.Resolve(created, corebilling.Record{
		AnchorDay:              22,
		PlanSlug:               corebilling.SlugCustom,
		FlatFeeCents:           40_000,
		RateCentsPerMillion:    3_000,
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

// The trial date is kept once past, like the contract date, so a client can say
// when the trial ended rather than only when it will.
func TestResolveKeepsTheTrialDateAfterItPasses(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, nil, later, true)

	if ent.Status != corebilling.StatusFree {
		t.Fatalf("status = %s, want FREE", ent.Status)
	}
	if want := created.AddDate(0, 0, corebilling.TrialDays); !ent.TrialEndsAt.Equal(want) {
		t.Errorf("trial_ends_at = %s, want it preserved at %s", ent.TrialEndsAt, want)
	}
}

// Retention is the same term on every card now, and a deal's is its own.
func TestResolveReportsRetention(t *testing.T) {
	if got := retention(t, corebilling.Resolve(created, corebilling.Record{}, nil, later, true)); got != corebilling.CardRetentionDays {
		t.Errorf("retention with no row = %d days, want the card's %d", got, corebilling.CardRetentionDays)
	}
	if got := retention(t, corebilling.Resolve(created, deal(), nil, later, true)); got != corebilling.CardRetentionDays {
		t.Errorf("a deal's retention = %d days, want %d", got, corebilling.CardRetentionDays)
	}

	// A negotiated retention is the org's own, and it expires with the deal — the
	// same rule the allowance follows, because both are terms of one agreement.
	rec := deal()
	rec.RetentionDaysOverride = 10 * corebilling.RetentionYearDays
	if got := retention(t, corebilling.Resolve(created, rec, nil, later, true)); got != 3_650 {
		t.Errorf("retention = %d days, want the negotiated 3650", got)
	}
	rec.ContractEndsAt = later.Add(-time.Hour)
	if got := retention(t, corebilling.Resolve(created, rec, nil, later, true)); got != corebilling.CardRetentionDays {
		t.Errorf("retention after the deal ended = %d days, want the card's %d", got, corebilling.CardRetentionDays)
	}
}

// An unknown card bounds nothing rather than imposing the current card's term on
// a customer who was sold another.
func TestResolveUnknownCardHasNoRetentionBound(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2019-01-1",
	}, nil, later, true)
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days for an unknown card, want none", *ent.RetentionDays)
	}

	withOverride := corebilling.Resolve(created, corebilling.Record{
		Present: true, PlanSlug: "usage-2019-01-1", RetentionDaysOverride: 900,
	}, nil, later, true)
	if got := retention(t, withOverride); got != 900 {
		t.Errorf("retention = %d days, want the negotiated 900", got)
	}
}

func retention(t *testing.T, ent corebilling.Entitlement) int64 {
	t.Helper()
	if ent.RetentionDays == nil {
		t.Fatalf("entitlement has no retention bound; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.RetentionDays
}

func flatten(e corebilling.Entitlement) string {
	return fmt.Sprintf("%s/%s/%s/%s allowance=%v retention=%v card=%v terms=%v chargeable=%v "+
		"trial=%s contract=%s window=[%s,%s) enabled=%v",
		e.Slug, e.DisplayName, e.Currency, e.Status, str(e.IncludedEvents), str(e.RetentionDays),
		e.Card, e.Terms, e.Chargeable, e.TrialEndsAt, e.ContractEndsAt,
		e.PeriodStart, e.PeriodEnd, e.BillingEnabled)
}

func str(v *int64) any {
	if v == nil {
		return "none"
	}
	return *v
}
