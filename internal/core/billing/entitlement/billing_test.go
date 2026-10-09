package entitlement_test

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

var (
	// Created on the 10th, so a window with no anchor override runs 10th to 10th.
	created = time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)
	// Months after signup.
	later = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	billingOn  = corebilling.Config{Enabled: true}
	billingOff = corebilling.Config{}
)

func quota(t *testing.T, ent entitlement.Entitlement) int64 {
	t.Helper()
	if ent.IncludedEvents == nil {
		t.Fatalf("entitlement has no allowance; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.IncludedEvents
}

// What an org with no subscription resolves to: the current plan's allowance, and
// a year of history whatever that plan keeps.
func freeEvents() int64    { return entitlement.CurrentPlan().FreeEvents }
func freeRetention() int64 { return entitlement.RetentionYearDays }

// An org with no row is the ordinary case: free, on the current plan's allowance.
func TestResolveNoRowIsFree(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, nil, later, billingOn)
	if ent.Status != entitlement.StatusFree || ent.Slug != entitlement.SlugFree {
		t.Errorf("resolved %s/%s, want FREE/free", ent.Status, ent.Slug)
	}
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance = %d, want the current plan's %d", got, freeEvents())
	}
}

// The quota window is the org's billing anniversary, not the 1st, and a plan
// never moves it — that is what keeps it equal to the window the meter sums.
func TestResolveWindowRunsFromTheAnniversary(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, nil, later, billingOn)
	wantStart := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !ent.PeriodStart.Equal(wantStart) {
		t.Errorf("period_start = %s, want %s (the org signed up on the 10th)", ent.PeriodStart, wantStart)
	}

	// A contract end is the end of the AGREEMENT, and must not shorten the window.
	withContract := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		ContractEndsAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}, nil, later, billingOn)
	if !withContract.PeriodStart.Equal(ent.PeriodStart) || !withContract.PeriodEnd.Equal(ent.PeriodEnd) {
		t.Errorf("a contract moved the quota window to [%s, %s), want [%s, %s)",
			withContract.PeriodStart, withContract.PeriodEnd, ent.PeriodStart, ent.PeriodEnd)
	}

	// An explicit anchor overrides the signup day.
	anchored := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree, AnchorDay: 22,
	}, nil, later, billingOn)
	if want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC); !anchored.PeriodStart.Equal(want) {
		t.Errorf("anchored period_start = %s, want %s", anchored.PeriodStart, want)
	}
}

// A lapsed comp falls back to the current allowance WITH the free name. Keeping the
// negotiated number after the deal ended is the one bug here that costs money.
func TestResolveDropsAnExpiredCompToTheAllowance(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ContractEndsAt:         later.Add(-time.Hour),
	}

	ent := entitlement.Resolve(created, rec, nil, later, billingOn)
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status after the comp ended = %s, want FREE", ent.Status)
	}
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance after the comp ended = %d, want the current plan's %d", got, freeEvents())
	}
	if ent.DisplayName != entitlement.FreeDisplayName {
		t.Errorf("display name after the comp ended = %q, want %q", ent.DisplayName, entitlement.FreeDisplayName)
	}
	// The date stays visible: it is now the answer to "when did this end".
	if !ent.ContractEndsAt.Equal(rec.ContractEndsAt) {
		t.Errorf("contract_ends_at = %s, want it preserved after expiry", ent.ContractEndsAt)
	}

	// One tick before it ends, the comp is still fully in force.
	stillLive := entitlement.Resolve(created, rec, nil, rec.ContractEndsAt.Add(-time.Nanosecond), billingOn)
	if quota(t, stillLive) != 5_000_000 || stillLive.DisplayName != "Acme Enterprise" {
		t.Errorf("just before expiry: allowance=%d name=%q, want 5000000 and the negotiated name",
			quota(t, stillLive), stillLive.DisplayName)
	}
}

// An operator names the last day a deal runs; Resolve compares half-open. The
// conversion between the two is what keeps the org from losing the day it was
// given, so it is pinned against Resolve rather than on its own.
func TestContractEndExclusiveCoversTheWholeNamedDay(t *testing.T) {
	lastDay := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree, IncludedEventsOverride: 5_000_000,
		ContractEndsAt: entitlement.ContractEndExclusive(lastDay),
	}

	lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := quota(t, entitlement.Resolve(created, rec, nil, lateOnTheLastDay, billingOn)); got != 5_000_000 {
		t.Errorf("allowance at %s = %d, want the comp's — the named day is inclusive", lateOnTheLastDay, got)
	}
	nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := quota(t, entitlement.Resolve(created, rec, nil, nextMidnight, billingOn)); got != freeEvents() {
		t.Errorf("allowance at %s = %d, want the current plan's — the comp ends when the day does", nextMidnight, got)
	}
}

// A time-boxed comp is stored on free, so its overrides end when its contract does.
// Without this a comped pilot never expires.
func TestResolveExpiresACompsOverrides(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.Add(-time.Hour),
	}
	if got := quota(t, entitlement.Resolve(created, rec, nil, later, billingOn)); got != freeEvents() {
		t.Errorf("allowance after the pilot ended = %d, want the current plan's %d", got, freeEvents())
	}

	rec.ContractEndsAt = later.AddDate(0, 1, 0)
	if got := quota(t, entitlement.Resolve(created, rec, nil, later, billingOn)); got != 5_000_000 {
		t.Errorf("allowance while the pilot runs = %d, want 5000000", got)
	}
}

func TestResolveAppliesNegotiatedOverrides(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug:           entitlement.SlugUsage,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
	}, liveSub(entitlement.SlugCustom), later, billingOn)

	if ent.Status != entitlement.StatusActive || ent.Slug != entitlement.SlugCustom {
		t.Errorf("resolved %s/%s, want ACTIVE/custom", ent.Status, ent.Slug)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("allowance = %d, want the negotiated 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want the negotiated name", ent.DisplayName)
	}
}

// Each override patches only its own field, so a comp that changed the allowance
// alone still shows free's name and retention.
func TestResolveOverridesAreIndependent(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree, IncludedEventsOverride: 2_000_000,
	}, nil, later, billingOn)

	if got := quota(t, ent); got != 2_000_000 {
		t.Errorf("allowance = %d, want the override", got)
	}
	if ent.DisplayName != entitlement.FreeDisplayName || retention(t, ent) != freeRetention() {
		t.Errorf("name/retention = %q/%d, want %q/%d", ent.DisplayName, retention(t, ent),
			entitlement.FreeDisplayName, freeRetention())
	}
}

// Switched off means a self-hosted install: no allowance anywhere, so no banner can
// fire even if a client ignores billing_enabled.
func TestResolveWithBillingOffHasNoQuota(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		IncludedEventsOverride: 5_000_000,
	}, nil, later, billingOff)

	if ent.BillingEnabled {
		t.Error("billing_enabled is true with the switch off")
	}
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d with billing off, want none", *ent.IncludedEvents)
	}
	// Same direction as the allowance: with no PUG_RETENTION_DAYS a self-hosted
	// install bounds nothing.
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d days with billing off, want none", *ent.RetentionDays)
	}
	if ent.TierUpTo != nil {
		t.Errorf("tiers = %v with billing off, want none", ent.TierUpTo)
	}
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status = %s with billing off, want FREE", ent.Status)
	}
	// The window is still real: usage is metered whether or not billing is on.
	if ent.PeriodStart.IsZero() || ent.PeriodEnd.IsZero() {
		t.Error("period bounds are missing with billing off")
	}
}

// A row naming a slug nothing knows any more is still just a row: it resolves free,
// and its own negotiated numbers still apply — they owe nothing to the catalog.
func TestResolveALegacyRowKeepsItsOverrides(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9",
	}, nil, later, billingOn)
	if ent.Status != entitlement.StatusFree || quota(t, ent) != freeEvents() || retention(t, ent) != freeRetention() {
		t.Errorf("resolved %s with %v / %v, want FREE on the current allowance and free's retention",
			ent.Status, ent.IncludedEvents, ent.RetentionDays)
	}

	withOverrides := entitlement.Resolve(created, entitlement.Record{
		Present: true, PlanSlug: "growth-v9", IncludedEventsOverride: 750_000, RetentionDaysOverride: 900,
	}, nil, later, billingOn)
	if got := quota(t, withOverrides); got != 750_000 {
		t.Errorf("allowance = %d, want the negotiated 750000", got)
	}
	if got := retention(t, withOverrides); got != 900 {
		t.Errorf("retention = %d days, want the negotiated 900", got)
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
				Present: true, PlanSlug: entitlement.SlugFree, IncludedEventsOverride: 5_000_000,
				ContractEndsAt: entitlement.ContractEndExclusive(tc.lastDay),
			}
			lateOnTheLastDay := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
			if got := quota(t, entitlement.Resolve(created, rec, nil, lateOnTheLastDay, billingOn)); got != 5_000_000 {
				t.Errorf("allowance at %s = %d, want the comp's — the named day is inclusive", lateOnTheLastDay, got)
			}
			nextMidnight := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			if got := quota(t, entitlement.Resolve(created, rec, nil, nextMidnight, billingOn)); got != freeEvents() {
				t.Errorf("allowance at %s = %d, want the current plan's — the comp ends when the day does", nextMidnight, got)
			}
		})
	}
}

// Present is the whole row's discriminator. A caller that builds a Record by
// hand and forgets it must get the same answer as one that passes none, rather
// than have its anchor day and overrides honoured.
func TestResolveIgnoresEveryFieldOfAnAbsentRow(t *testing.T) {
	absent := entitlement.Resolve(created, entitlement.Record{}, nil, later, billingOn)
	populated := entitlement.Resolve(created, entitlement.Record{
		AnchorDay:              22,
		PlanSlug:               entitlement.SlugCustom,
		ProviderProductID:      "prod_deal",
		IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride:  3_650,
		DisplayNameOverride:    "Acme Enterprise",
		ContractEndsAt:         later.AddDate(1, 0, 0),
	}, nil, later, billingOn)

	// Compared flattened: Entitlement holds pointers, so == would compare addresses.
	if flatten(populated) != flatten(absent) {
		t.Errorf("a Record with Present unset resolved to\n%s\nwant the no-row answer\n%s",
			flatten(populated), flatten(absent))
	}
}

// A paying org keeps its plan's retention, a deal its base plan's, and every other
// org free's year: every other status counts as free.
func TestResolveReportsThePlansRetention(t *testing.T) {
	plan := entitlement.CurrentPlan()
	for _, status := range corebilling.AllSubStatuses() {
		sub := liveSub(entitlement.SlugUsage)
		sub.Status = status
		want := freeRetention()
		if status.Live() {
			want = plan.RetentionDays
		}
		if got := retention(t, entitlement.Resolve(created, entitlement.Record{}, sub, later, billingOn)); got != want {
			t.Errorf("%s subscription: retention = %d days, want %d", status, got, want)
		}
	}
	deal := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug: plan.Slug,
	}
	if got := retention(t, entitlement.Resolve(created, deal, liveSub(entitlement.SlugCustom), later, billingOn)); got != plan.RetentionDays {
		t.Errorf("retention on a live deal = %d days, want its base plan's %d", got, plan.RetentionDays)
	}
}

// The org's own retention wins wherever it is set: no deal, contract, plan or switch
// gates it, unlike the allowance.
func TestResolveTheRetentionOverrideAppliesOnItsOwn(t *testing.T) {
	// Shorter than free's and the plan's, so a resolver taking the longer one fails.
	const days = 90
	comp := entitlement.Record{Present: true, PlanSlug: entitlement.SlugFree, RetentionDaysOverride: days}
	lapsed := comp
	lapsed.ContractEndsAt = later.Add(-time.Hour)
	deal := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug: entitlement.SlugUsage, RetentionDaysOverride: days,
	}
	for name, tc := range map[string]struct {
		rec entitlement.Record
		sub *entitlement.Subscription
		cfg corebilling.Config
	}{
		"on free":                    {rec: comp, cfg: billingOn},
		"past its contract":          {rec: lapsed, cfg: billingOn},
		"a staged deal":              {rec: deal, cfg: billingOn},
		"a deal beside a usage plan": {rec: deal, sub: liveSub(entitlement.SlugUsage), cfg: billingOn},
		"a live deal":                {rec: deal, sub: liveSub(entitlement.SlugCustom), cfg: billingOn},
		"an unknown plan":            {rec: comp, sub: liveSub("usage-2019-01"), cfg: billingOn},
		"billing off":                {rec: comp, cfg: billingOff},
		"billing off with a default": {rec: comp, cfg: corebilling.Config{RetentionDays: 30}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retention(t, entitlement.Resolve(created, tc.rec, tc.sub, later, tc.cfg)); got != days {
				t.Errorf("retention = %d days, want the override's %d", got, days)
			}
		})
	}
}

// Off, PUG_RETENTION_DAYS is every org's length, and unset keeps everything. On,
// billing decides and the default is ignored.
func TestResolveTakesTheDeploymentsRetentionOnlyWithBillingOff(t *testing.T) {
	off := entitlement.Resolve(created, entitlement.Record{}, liveSub(entitlement.SlugUsage), later,
		corebilling.Config{RetentionDays: 90})
	if got := retention(t, off); got != 90 {
		t.Errorf("retention with billing off = %d days, want PUG_RETENTION_DAYS's 90", got)
	}
	on := entitlement.Resolve(created, entitlement.Record{}, nil, later, corebilling.Config{Enabled: true, RetentionDays: 90})
	if got := retention(t, on); got != freeRetention() {
		t.Errorf("retention with billing on = %d days, want free's %d", got, freeRetention())
	}
	unknown := entitlement.Resolve(created, entitlement.Record{}, liveSub("usage-2019-01"), later,
		corebilling.Config{Enabled: true, RetentionDays: 90})
	if unknown.RetentionDays != nil {
		t.Errorf("retention on an unknown plan = %d days with billing on, want no bound", *unknown.RetentionDays)
	}
}

// A negative day count is a typo in the deployment, refused at wiring rather than
// read as either extreme.
func TestNewServiceRefusesANegativeRetention(t *testing.T) {
	// Construction never touches the pools, so nil reaches the check.
	if _, err := entitlement.NewService(nil, nil, corebilling.Config{RetentionDays: -1}); err == nil {
		t.Fatal("NewService accepted PUG_RETENTION_DAYS=-1")
	}
}

func retention(t *testing.T, ent entitlement.Entitlement) int64 {
	t.Helper()
	if ent.RetentionDays == nil {
		t.Fatalf("entitlement has no retention bound; want one (plan %q, status %s)", ent.Slug, ent.Status)
	}
	return *ent.RetentionDays
}

func str(v *int64) string {
	if v == nil {
		return "nil"
	}
	return strconv.FormatInt(*v, 10)
}

func flatten(e entitlement.Entitlement) string {
	return fmt.Sprintf("%s/%s/%s allowance=%v retention=%v tiers=%v contract=%s window=[%s,%s) enabled=%v",
		e.Slug, e.DisplayName, e.Status, str(e.IncludedEvents), str(e.RetentionDays), e.TierUpTo,
		e.ContractEndsAt, e.PeriodStart, e.PeriodEnd, e.BillingEnabled)
}

// Without a live subscription an org is free on the current plan's allowance: the
// row alone never makes it ACTIVE, because nothing would bill it.
func TestResolveWithNoSubscriptionIsFreeOnTheCurrentAllowance(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	plan := entitlement.CurrentPlan()
	for name, rec := range map[string]entitlement.Record{
		"no row":        {},
		"a free row":    {Present: true, PlanSlug: entitlement.SlugFree},
		"a staged deal": {Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal"},
		"a legacy row":  {Present: true, PlanSlug: "growth"},
	} {
		t.Run(name, func(t *testing.T) {
			ent := entitlement.Resolve(now.AddDate(0, -3, 0), rec, nil, now, billingOn)
			if ent.Status != entitlement.StatusFree || ent.Slug != entitlement.SlugFree || ent.DisplayName != "Free" {
				t.Fatalf("resolved %s/%s/%s, want FREE/free/Free", ent.Status, ent.Slug, ent.DisplayName)
			}
			if ent.IncludedEvents == nil || *ent.IncludedEvents != plan.FreeEvents {
				t.Errorf("IncludedEvents = %v, want %d", ent.IncludedEvents, plan.FreeEvents)
			}
			// Nothing bills a free org, so nothing splits its usage.
			if ent.TierUpTo != nil || ent.TierPlanSlug != "" {
				t.Errorf("TierUpTo = %v from %q, want none on free", ent.TierUpTo, ent.TierPlanSlug)
			}
		})
	}
}

// A deal splits over the plan pinned on its row, at its own product's rates. With
// no override it takes that plan's allowance, not nothing: a deal needs no quota.
func TestResolveADealSplitsOverItsBasePlan(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug: entitlement.SlugUsage,
	}
	sub := &entitlement.Subscription{PlanSlug: entitlement.SlugCustom, Status: corebilling.SubStatusActive}
	ent := entitlement.Resolve(now.AddDate(0, -3, 0), rec, sub, now, billingOn)
	if ent.Status != entitlement.StatusActive || ent.Slug != entitlement.SlugCustom {
		t.Fatalf("resolved %s/%s, want ACTIVE/custom", ent.Status, ent.Slug)
	}
	base, _ := entitlement.PlanBySlug(entitlement.SlugUsage)
	if ent.IncludedEvents == nil || *ent.IncludedEvents != base.FreeEvents ||
		!slices.Equal(ent.TierUpTo, base.TierUpTo) || ent.TierPlanSlug != base.Slug {
		t.Errorf("resolved %v / %v from %q, want the base plan's allowance and tiers",
			ent.IncludedEvents, ent.TierUpTo, ent.TierPlanSlug)
	}
}

// A deal pinned to a plan the catalog lost is an unknown plan like any other: no
// allowance, retention or tiers, rather than a guess at the current plan's.
func TestResolveADealOnAnUnknownBasePlanHasNoTiers(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug: "usage-2019-01", IncludedEventsOverride: 5_000_000,
	}
	sub := &entitlement.Subscription{PlanSlug: entitlement.SlugCustom, Status: corebilling.SubStatusActive}
	ent := entitlement.Resolve(now.AddDate(0, -3, 0), rec, sub, now, billingOn)
	if ent.Status != entitlement.StatusActive || ent.Slug != entitlement.SlugCustom ||
		ent.IncludedEvents != nil || ent.RetentionDays != nil || ent.TierUpTo != nil {
		t.Fatalf("resolved %+v, want ACTIVE/custom with no allowance, retention or tiers", ent)
	}
}

// The catalog dropped a slug a paying org still holds: no allowance, retention or
// tiers, which the meter reports rather than guess a split. A comp's allowance does
// not supply one either: an allowance with nothing to split it by is exactly the
// guess.
func TestResolveAnUnknownSubscriptionPlanHasNoTiers(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	sub := &entitlement.Subscription{PlanSlug: "usage-2019-01", Status: corebilling.SubStatusActive}
	for name, rec := range map[string]entitlement.Record{
		"no row": {},
		"a comp": {
			Present: true, PlanSlug: entitlement.SlugFree,
			IncludedEventsOverride: 1_000_000, DisplayNameOverride: "Beta",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ent := entitlement.Resolve(now.AddDate(0, -3, 0), rec, sub, now, billingOn)
			if ent.Status != entitlement.StatusActive || ent.Slug != "usage-2019-01" ||
				ent.DisplayName != "usage-2019-01" || ent.IncludedEvents != nil ||
				ent.RetentionDays != nil || ent.TierUpTo != nil {
				t.Fatalf("resolved %+v, want ACTIVE on its own slug with no allowance, retention or tiers", ent)
			}
		})
	}
}
