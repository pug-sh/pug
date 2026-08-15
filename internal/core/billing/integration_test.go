package billing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

const actor = "tester@localhost"

type fixture struct {
	svc   *corebilling.Service
	pg    *testutil.TestPostgres
	orgID string
}

// Orgs are backdated well past the trial so a test asserting a granted plan is
// not also fighting a live trial window.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))

	return &fixture{
		svc:   corebilling.NewService(pg.PgRO, pg.PgW, true),
		pg:    pg,
		orgID: org.ID,
	}
}

func TestOrgWithNoRowResolvesFromItsAgeAndWritesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusFree || ent.Slug != corebilling.SlugFree {
		t.Errorf("status/slug = %s/%s, want FREE/free for an org past its trial", ent.Status, ent.Slug)
	}

	// A read must not materialize a row: "no row" is the normal state, and one
	// appearing here would make the trial stored rather than derived.
	var rows int
	if err := f.pg.PgRO.QueryRow(t.Context(),
		"select count(*) from billing_entitlements where org_id = $1", f.orgID).Scan(&rows); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	if rows != 0 {
		t.Errorf("a read created %d entitlement rows, want 0", rows)
	}
}

// The assertion that matters most in this slice: the window billing reports and
// the window the meter sums are the same one. A client renders "X of Y" from two
// separate RPCs, so a divergence here makes both numbers individually correct and
// the sentence wrong.
func TestQuotaWindowMatchesTheMeters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	meter := coreusage.NewReader(f.pg.PgRO)

	for _, tc := range []struct {
		name   string
		anchor int
	}{
		{"derived from the signup day", 0},
		{"an explicit mid-month anchor", 22},
		{"an anchor that clamps in short months", 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := corebilling.Change{PlanSlug: "growth"}
			if tc.anchor > 0 {
				change.AnchorDay = corebilling.Set(tc.anchor)
			}
			if _, err := f.svc.SetPlan(ctx, f.orgID, actor, change); err != nil {
				t.Fatalf("SetPlan: %v", err)
			}

			// Every month of a year, so a clamped February is covered rather than
			// whichever month the suite happens to run in.
			for month := time.January; month <= time.December; month++ {
				now := time.Date(2026, month, 14, 9, 30, 0, 0, time.UTC)

				ent, err := f.svc.GetEntitlement(ctx, f.orgID, now)
				if err != nil {
					t.Fatalf("GetEntitlement: %v", err)
				}
				meterStart, meterEnd, err := meter.GetOrgPeriod(ctx, f.orgID, now)
				if err != nil {
					t.Fatalf("GetOrgPeriod: %v", err)
				}
				if !ent.PeriodStart.Equal(meterStart) || !ent.PeriodEnd.Equal(meterEnd) {
					t.Fatalf("%s: billing window [%s, %s) != meter window [%s, %s)",
						month, ent.PeriodStart, ent.PeriodEnd, meterStart, meterEnd)
				}
			}
		})
	}
}

func TestGetEntitlementReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.GetEntitlement(t.Context(), xid.New().String(), time.Now())
	if !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Errorf("err = %v, want ErrOrgNotFound", err)
	}
}

func TestSetPlanRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       "growth",
		ContractEndsAt: corebilling.Set(until),
		Note:           corebilling.Set("annual wire, INV-123"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusActive || ent.Slug != "growth" {
		t.Errorf("status/slug = %s/%s, want ACTIVE/growth", ent.Status, ent.Slug)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != 500_000 {
		t.Errorf("quota = %v, want the catalog's 500000", ent.IncludedEvents)
	}
	if !ent.ContractEndsAt.Equal(until) {
		t.Errorf("contract_ends_at = %s, want %s", ent.ContractEndsAt, until)
	}
}

// The reason un-passed flags leave stored values alone: the common re-set is a
// renewal, and reverting a negotiated quota to a catalog number would be silent.
func TestReSetKeepsUnmentionedOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	price := int64(40_000)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		IncludedEvents: corebilling.Set(int64(5_000_000)),
		DisplayName:    corebilling.Set("Acme Enterprise"),
		PriceCents:     corebilling.Set(price),
		Currency:       corebilling.Set("USD"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	// A renewal: a new end date and nothing else.
	rec, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		ContractEndsAt: corebilling.Set(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("renewal SetPlan: %v", err)
	}
	if rec.IncludedEventsOverride != 5_000_000 {
		t.Errorf("quota after a renewal = %d, want the negotiated 5000000 preserved", rec.IncludedEventsOverride)
	}
	if rec.DisplayNameOverride != "Acme Enterprise" {
		t.Errorf("name after a renewal = %q, want it preserved", rec.DisplayNameOverride)
	}
	if rec.PriceCentsOverride == nil || *rec.PriceCentsOverride != price {
		t.Errorf("price after a renewal = %v, want it preserved", rec.PriceCentsOverride)
	}

	// And an explicit clear really clears.
	cleared, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       "growth",
		IncludedEvents: corebilling.Clear[int64](),
		DisplayName:    corebilling.Clear[string](),
	})
	if err != nil {
		t.Fatalf("clearing SetPlan: %v", err)
	}
	if cleared.IncludedEventsOverride != 0 || cleared.DisplayNameOverride != "" {
		t.Errorf("cleared overrides = %d/%q, want empty", cleared.IncludedEventsOverride, cleared.DisplayNameOverride)
	}
}

// The database is the guard, not the CLI: the row is what every read trusts.
func TestCustomPlanRequiresAQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugCustom})
	if !errors.Is(err, corebilling.ErrCustomNeedsQuota) {
		t.Fatalf("err = %v, want ErrCustomNeedsQuota", err)
	}

	// Straight past the service, to prove the constraint itself holds.
	_, err = f.pg.PgW.Exec(t.Context(),
		"insert into billing_entitlements (org_id, plan_slug) values ($1, 'custom')", f.orgID)
	if err == nil {
		t.Error("the database accepted a custom entitlement with no quota")
	} else if !strings.Contains(err.Error(), "billing_entitlements_custom_needs_quota") {
		t.Errorf("err = %v, want the custom_needs_quota constraint", err)
	}
}

func TestPriceOverrideRequiresACurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:   "growth",
		PriceCents: corebilling.Set(int64(40_000)),
	})
	if !errors.Is(err, corebilling.ErrPriceNeedsCurrency) {
		t.Fatalf("err = %v, want ErrPriceNeedsCurrency", err)
	}

	_, err = f.pg.PgW.Exec(t.Context(),
		"insert into billing_entitlements (org_id, plan_slug, currency_override) values ($1, 'growth', 'USD')", f.orgID)
	if err == nil {
		t.Error("the database accepted a currency with no amount")
	} else if !strings.Contains(err.Error(), "billing_entitlements_currency_needs_price") {
		t.Errorf("err = %v, want the currency_needs_price constraint", err)
	}
}

// Every slug the Go catalog knows must be storable. This is the test that fails
// when the catalog and the column's check constraint drift apart.
func TestEveryCatalogSlugIsStorable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, plan := range corebilling.Plans() {
		change := corebilling.Change{PlanSlug: plan.Slug}
		if plan.Slug == corebilling.SlugCustom {
			change.IncludedEvents = corebilling.Set(int64(1))
		}
		if plan.Slug == corebilling.SlugTrial {
			continue // not settable by design; ExtendTrial owns it
		}
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
			t.Errorf("%s: %v — the catalog and the plan_slug check constraint have drifted", plan.Slug, err)
		}
	}
}

// The custom tier is never purchasable, but an operator must be able to grant it
// to an org that has never held one — that is what a negotiated deal IS.
func TestCustomPlanIsGrantableToAnyOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		IncludedEvents: corebilling.Set(int64(5_000_000)),
	}); err != nil {
		t.Fatalf("granting a custom deal: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != 5_000_000 {
		t.Errorf("quota = %v, want the negotiated 5000000", ent.IncludedEvents)
	}
}

func TestExtendTrialAndClear(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	now := time.Now()

	if _, err := f.svc.ExtendTrial(ctx, f.orgID, actor, 30, now); err != nil {
		t.Fatalf("ExtendTrial: %v", err)
	}
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusTrialing {
		t.Errorf("status = %s after extend-trial, want TRIALING", ent.Status)
	}
	if got := ent.TrialEndsAt.Sub(now).Hours(); got < 29*24 || got > 31*24 {
		t.Errorf("trial ends in %.0fh, want about 30 days", got)
	}

	if err := f.svc.Clear(ctx, f.orgID, actor); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	ent, err = f.svc.GetEntitlement(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("GetEntitlement after clear: %v", err)
	}
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s after clear, want FREE (the org is back on the derived floors)", ent.Status)
	}
}

func TestHistoryRecordsEveryChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug: "starter", Note: corebilling.Set("first"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := f.svc.SetPlan(ctx, f.orgID, "someone@else", corebilling.Change{
		PlanSlug: "scale", Note: corebilling.Set("upgrade"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if err := f.svc.Clear(ctx, f.orgID, actor); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	entries, err := f.svc.History(ctx, f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("history has %d entries, want 3 (two grants and a clear)", len(entries))
	}
	// Newest first, and the clear is a snapshot with no values.
	if entries[0].Record.Present {
		t.Errorf("the newest entry has values; a clear must record an empty snapshot")
	}
	if entries[1].Record.PlanSlug != "scale" || entries[1].Actor != "someone@else" {
		t.Errorf("entry 1 = %s by %s, want scale by someone@else",
			entries[1].Record.PlanSlug, entries[1].Actor)
	}
	if entries[2].Record.PlanSlug != "starter" || entries[2].Record.Note != "first" {
		t.Errorf("entry 2 = %s/%q, want starter/first", entries[2].Record.PlanSlug, entries[2].Record.Note)
	}
}

// A refused change must leave nothing behind: the row and its history commit
// together or not at all.
func TestRejectedChangeAppendsNoHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor,
		corebilling.Change{PlanSlug: corebilling.SlugCustom}); err == nil {
		t.Fatal("a custom plan with no quota was accepted")
	}

	entries, err := f.svc.History(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("history has %d entries after a refused change, want 0", len(entries))
	}
}

// The history has no foreign key on purpose: "what were they on when they left"
// is asked after the org is gone, usually in a refund dispute.
func TestHistorySurvivesTheOrgBeingDeleted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{PlanSlug: "scale"}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := f.pg.PgW.Exec(ctx, "delete from orgs where id = $1", f.orgID); err != nil {
		t.Fatalf("delete org: %v", err)
	}

	// The entitlement cascaded away with the org...
	var live int
	if err := f.pg.PgRO.QueryRow(ctx,
		"select count(*) from billing_entitlements where org_id = $1", f.orgID).Scan(&live); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	if live != 0 {
		t.Errorf("%d entitlement rows outlived the org, want 0", live)
	}

	// ...and the record of what they held did not.
	entries, err := f.svc.History(ctx, f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 1 || entries[0].Record.PlanSlug != "scale" {
		t.Errorf("history after deletion = %+v, want the scale grant preserved", entries)
	}
}

func TestSetPlanReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), xid.New().String(), actor, corebilling.Change{PlanSlug: "growth"})
	if !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Errorf("err = %v, want ErrOrgNotFound", err)
	}
}

func TestSetPlanRefusesTheTrialSlug(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugTrial})
	if !errors.Is(err, corebilling.ErrTrialNotSettable) {
		t.Errorf("err = %v, want ErrTrialNotSettable", err)
	}
}
