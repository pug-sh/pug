package entitlement_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

const actor = "tester@localhost"

type fixture struct {
	svc   *entitlement.Service
	pg    *testutil.TestPostgres
	orgID string
}

// Orgs are backdated to a fixed date, so the anchor and every period derived from
// it do not depend on the day the suite runs.
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

	svc, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &fixture{
		svc:   svc,
		pg:    pg,
		orgID: org.ID,
	}
}

func TestOrgWithNoRowResolvesFreeAndWritesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != entitlement.StatusFree || ent.Slug != entitlement.SlugFree {
		t.Errorf("status/slug = %s/%s, want FREE/free for an org with no row", ent.Status, ent.Slug)
	}

	// A read must not materialize a row: "no row" is the normal state, and one
	// appearing here would make free stored rather than derived.
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
	meter := coreusage.NewService(f.pg.PgRO, f.pg.PgW)

	for _, tc := range []struct {
		name   string
		anchor int
	}{
		{"derived from the signup day", 0},
		{"an explicit mid-month anchor", 22},
		{"an anchor that clamps in short months", 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := entitlement.Change{PlanSlug: entitlement.SlugFree}
			if tc.anchor > 0 {
				change.AnchorDay = new(tc.anchor)
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
	if !errors.Is(err, entitlement.ErrOrgNotFound) {
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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		ProviderProductID: new("prod_deal"),
		ContractEndsAt:    new(until),
		Note:              new("INV-123"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	// Staged, not bought: nothing bills it yet, so it resolves free.
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != entitlement.StatusFree {
		t.Errorf("status = %s for a deal nobody has bought, want FREE", ent.Status)
	}
	if !ent.ContractEndsAt.Equal(until) {
		t.Errorf("contract_ends_at = %s, want %s", ent.ContractEndsAt, until)
	}
	rec, err := f.svc.StoredRecord(ctx, f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.PlanSlug != entitlement.SlugCustom || rec.ProviderProductID != "prod_deal" || rec.Note != "INV-123" {
		t.Errorf("stored %+v, want the deal, its product and its note", rec)
	}
}

// A deal's retention is stored beside its quota and resolves the same way, which
// is the whole reason it is a column rather than prose in the note.
func TestNegotiatedRetentionRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		ProviderProductID: new("prod_deal"),
		IncludedEvents:    new(int64(5_000_000)),
		RetentionDays:     new(int64(10 * entitlement.RetentionYearDays)),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	// A deal's terms are held through its own subscription.
	seedLiveCustomSubscription(t, f.pg, f.orgID)

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.RetentionDays == nil || *ent.RetentionDays != 3_650 {
		t.Errorf("retention = %v, want the negotiated 3650", ent.RetentionDays)
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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		ProviderProductID: new("prod_deal"),
		IncludedEvents:    new(int64(5_000_000)),
		RetentionDays:     new(int64(3_650)),
		DisplayName:       new("Acme Enterprise"),
		AnchorDay:         new(17),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	// A renewal: a new end date and nothing else.
	rec, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugCustom,
		ContractEndsAt: new(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("renewal SetPlan: %v", err)
	}
	if rec.IncludedEventsOverride != 5_000_000 {
		t.Errorf("quota after a renewal = %d, want the negotiated 5000000 preserved", rec.IncludedEventsOverride)
	}
	if rec.RetentionDaysOverride != 3_650 {
		t.Errorf("retention after a renewal = %d, want the negotiated 3650 preserved", rec.RetentionDaysOverride)
	}
	if rec.DisplayNameOverride != "Acme Enterprise" {
		t.Errorf("name after a renewal = %q, want it preserved", rec.DisplayNameOverride)
	}
	if rec.AnchorDay != 17 {
		t.Errorf("anchor day after a renewal = %d, want it preserved", rec.AnchorDay)
	}

	// And an explicit clear really clears — and clears only what it names. (A deal's
	// allowance is not clearable: see TestADealsAllowanceIsNotClearedToTheDefault.)
	cleared, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:      entitlement.SlugCustom,
		RetentionDays: new(int64),
		DisplayName:   new(string),
	})
	if err != nil {
		t.Fatalf("clearing SetPlan: %v", err)
	}
	if cleared.IncludedEventsOverride != 5_000_000 || cleared.DisplayNameOverride != "" || cleared.RetentionDaysOverride != 0 {
		t.Errorf("after clearing retention and name: %d/%q/%d, want 5000000/\"\"/0",
			cleared.IncludedEventsOverride, cleared.DisplayNameOverride, cleared.RetentionDaysOverride)
	}
}

// The database is the guard, not the CLI: the row is what every read trusts. A
// deal's price is its product, so a deal without one — or a product without a
// deal — is refused below the service too.
func TestCustomPlanRequiresAProduct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugCustom})
	if !errors.Is(err, entitlement.ErrCustomNeedsProduct) {
		t.Fatalf("err = %v, want ErrCustomNeedsProduct", err)
	}

	// Straight past the service, to prove each constraint itself holds both ways: a
	// deal is exactly a product and a base plan.
	for name, tc := range map[string]struct{ insert, constraint string }{
		"a deal with no product": {
			"insert into billing_entitlements (org_id, plan_slug, base_plan_slug) values ($1, 'custom', 'usage-2026-10')",
			"billing_entitlements_custom_needs_product",
		},
		"a product on free": {
			"insert into billing_entitlements (org_id, plan_slug, provider_product_id) values ($1, 'free', 'prod_x')",
			"billing_entitlements_custom_needs_product",
		},
		"a deal with no base plan": {
			"insert into billing_entitlements (org_id, plan_slug, provider_product_id) values ($1, 'custom', 'prod_x')",
			"billing_entitlements_custom_needs_base_plan",
		},
		"a base plan on free": {
			"insert into billing_entitlements (org_id, plan_slug, base_plan_slug) values ($1, 'free', 'usage-2026-10')",
			"billing_entitlements_custom_needs_base_plan",
		},
	} {
		_, err = f.pg.PgW.Exec(t.Context(), tc.insert, f.orgID)
		var pgErr *pgconn.PgError
		if err == nil {
			t.Errorf("%s: the database accepted it", name)
		} else if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: err = %v, want %s", name, err, tc.constraint)
		}
	}
}

// A catalog plan is held only through a subscription, so the service refuses to
// set one — but its slug must still fit the columns that store it:
// billing_subscriptions.plan_slug and a deal's base_plan_slug carry no check
// constraint, so this is what catches a slug outgrowing varchar(50).
func TestEveryCatalogSlugIsStorableButNotAssignable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, plan := range entitlement.Plans() {
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: plan.Slug}); !errors.Is(err, entitlement.ErrPlanNotAssignable) {
			t.Errorf("SetPlan(%s) = %v, want ErrPlanNotAssignable", plan.Slug, err)
		}
		if _, err := f.pg.PgW.Exec(t.Context(),
			`insert into billing_subscriptions (currency, id, org_id, plan_slug, price_cents, provider,
			   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
			 values ('USD', $1, $2, $3, 100, 'dodo', 'cus_1', 'expired', $4, now(), 'expired')`,
			xid.New().String(), f.orgID, plan.Slug, "sub_"+plan.Slug); err != nil {
			t.Errorf("%s: %v — this catalog slug is not storable", plan.Slug, err)
		}
	}
	for _, change := range []entitlement.Change{
		{PlanSlug: entitlement.SlugFree},
		{PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal")},
	} {
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
			t.Errorf("SetPlan(%s): %v", change.PlanSlug, err)
		}
	}
}

// An operator must be able to stage a deal for an org that has never held one —
// that is what a negotiated deal IS. Staged is not held: until its subscription
// lands, the org stays plain free and the terms wait on its row.
func TestCustomPlanIsGrantableToAnyOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		ProviderProductID: new("prod_deal"),
		IncludedEvents:    new(int64(5_000_000)),
		DisplayName:       new("Acme Enterprise"),
	}); err != nil {
		t.Fatalf("granting a custom deal: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != entitlement.StatusFree || ent.Slug != entitlement.SlugFree ||
		ent.DisplayName != entitlement.FreeDisplayName || ent.IncludedEvents == nil ||
		*ent.IncludedEvents != entitlement.CurrentPlan().FreeEvents {
		t.Errorf("a staged deal resolved %s/%s %q on %v, want plain free", ent.Status, ent.Slug, ent.DisplayName, ent.IncludedEvents)
	}
	rec, err := f.svc.StoredRecord(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.PlanSlug != entitlement.SlugCustom || rec.IncludedEventsOverride != 5_000_000 {
		t.Errorf("stored %+v, want the deal's terms waiting on the row", rec)
	}
}

// History has its own hand-written row->Record mapper, the fifth copy of the same
// field list: a field dropped from that copy loses a deal's terms silently while
// every other path still round-trips.
func TestHistoryRoundTripsEveryOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Hour)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		IncludedEvents:    new(int64(5_000_000)),
		RetentionDays:     new(int64(2555)),
		DisplayName:       new("Acme Enterprise"),
		AnchorDay:         new(11),
		ContractEndsAt:    &until,
		ProviderProductID: new("prod_acme"),
		Note:              new("$400/mo, INV-123"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	entries, err := f.svc.History(ctx, f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("history has %d entries, want 1", len(entries))
	}
	got := entries[0].Record
	if got.PlanSlug != entitlement.SlugCustom || got.IncludedEventsOverride != 5_000_000 ||
		got.RetentionDaysOverride != 2555 || got.DisplayNameOverride != "Acme Enterprise" ||
		got.AnchorDay != 11 || got.ProviderProductID != "prod_acme" ||
		got.BasePlanSlug != entitlement.CurrentPlan().Slug ||
		got.Note != "$400/mo, INV-123" || !got.ContractEndsAt.Equal(until) {
		t.Errorf("history record = %+v, want every override the grant named, and its pin", got)
	}
}

// The trial is gone, but a snapshot recorded while it ran keeps its end: history
// answers what was true then.
func TestHistoryKeepsALegacyTrialEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	trialEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlement_history (actor, id, org_id, plan_slug, trial_ends_at)
		 values ('ops', $1, $2, 'trial', $3)`, xid.New().String(), f.orgID, trialEnd); err != nil {
		t.Fatalf("seed an extend-trial snapshot: %v", err)
	}
	entries, err := f.svc.History(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 1 || !entries[0].TrialEndsAt.Equal(trialEnd) {
		t.Fatalf("history = %+v, want the snapshot with its trial end", entries)
	}
}

func TestHistoryRecordsEveryChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugFree, Note: new("first"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := f.svc.SetPlan(ctx, f.orgID, "someone@else", entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"), Note: new("upgrade"),
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
	if entries[1].Record.PlanSlug != entitlement.SlugCustom || entries[1].Actor != "someone@else" {
		t.Errorf("entry 1 = %s by %s, want custom by someone@else",
			entries[1].Record.PlanSlug, entries[1].Actor)
	}
	if entries[2].Record.PlanSlug != entitlement.SlugFree || entries[2].Record.Note != "first" {
		t.Errorf("entry 2 = %s/%q, want free/first", entries[2].Record.PlanSlug, entries[2].Record.Note)
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
		entitlement.Change{PlanSlug: entitlement.SlugCustom}); err == nil {
		t.Fatal("a custom plan with no product was accepted")
	}

	entries, err := f.svc.History(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("history has %d entries after a refused change, want 0", len(entries))
	}
}

// The column's check rejects "" alone, so the blank is the case that would get
// through and store an entry nobody can be asked about.
func TestBlankActorIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, blank := range []string{"", " ", "\t\n"} {
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, blank,
			entitlement.Change{PlanSlug: entitlement.SlugFree}); !errors.Is(err, entitlement.ErrActorRequired) {
			t.Errorf("SetPlan(%q) = %v, want ErrActorRequired", blank, err)
		}
		if err := f.svc.Clear(t.Context(), f.orgID, blank); !errors.Is(err, entitlement.ErrActorRequired) {
			t.Errorf("Clear(%q) = %v, want ErrActorRequired", blank, err)
		}
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
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
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
	if len(entries) != 1 || entries[0].Record.PlanSlug != entitlement.SlugFree {
		t.Errorf("history after deletion = %+v, want the free row preserved", entries)
	}
}

func TestSetPlanReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), xid.New().String(), actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	if !errors.Is(err, entitlement.ErrOrgNotFound) {
		t.Errorf("err = %v, want ErrOrgNotFound", err)
	}
}

func TestSetPlanAssignsOnlyFreeAndCustom(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugUsage}); !errors.Is(err, entitlement.ErrPlanNotAssignable) {
		t.Errorf("set usage: err = %v, want ErrPlanNotAssignable", err)
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: "trial"}); !errors.Is(err, entitlement.ErrPlanNotFound) {
		t.Errorf("set trial: err = %v, want ErrPlanNotFound", err)
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
		t.Errorf("set free: %v", err)
	}
}

func TestADealIsExactlyAProduct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	product := "prod_deal"
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugCustom}); !errors.Is(err, entitlement.ErrCustomNeedsProduct) {
		t.Errorf("custom without a product: err = %v, want ErrCustomNeedsProduct", err)
	}
	until := time.Now().AddDate(0, 1, 0)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugFree, ProviderProductID: &product, ContractEndsAt: &until,
	}); !errors.Is(err, entitlement.ErrProductNeedsCustom) {
		t.Errorf("a product on a free comp: err = %v, want ErrProductNeedsCustom", err)
	}
	// No quota needed: a deal may charge from its product's first tier.
	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugCustom, ProviderProductID: &product})
	if err != nil || rec.IncludedEventsOverride != 0 {
		t.Fatalf("custom with a product and no quota: rec = %+v, err = %v", rec, err)
	}
	// free without --until clears the product with the contract and overrides.
	rec, err = f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	if err != nil || rec.ProviderProductID != "" {
		t.Fatalf("back to free: rec = %+v, err = %v; the product must be cleared", rec, err)
	}
}

func TestADealWithALiveSubscriptionCannotLeaveCustom(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	product := "prod_deal"
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugCustom, ProviderProductID: &product}); err != nil {
		t.Fatalf("stage deal: %v", err)
	}
	seedLiveCustomSubscription(t, f.pg, f.orgID)
	// A renewal on unchanged terms, no quota, is not a stranding.
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugCustom}); err != nil {
		t.Errorf("renew the live deal: %v", err)
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); !errors.Is(err, entitlement.ErrClearWouldStrandSubscription) {
		t.Errorf("leave custom under a live deal: err = %v, want ErrClearWouldStrandSubscription", err)
	}
	// The live subscription's renewals map through the stored product, so swapping it
	// strands the deal as surely as leaving custom does.
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal_v2"),
	}); !errors.Is(err, entitlement.ErrClearWouldStrandSubscription) {
		t.Errorf("swap the product under a live deal: err = %v, want ErrClearWouldStrandSubscription", err)
	}
}

// A deal splits over the plan current when its product is set, pinned on its row
// and recorded in its history; leaving custom unpins it with the product.
func TestADealIsPinnedWhenItsProductIsSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"),
	})
	if err != nil {
		t.Fatalf("stage deal: %v", err)
	}
	if rec.BasePlanSlug != entitlement.CurrentPlan().Slug {
		t.Errorf("base plan = %q, want the current plan %q", rec.BasePlanSlug, entitlement.CurrentPlan().Slug)
	}
	rec, err = f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	if err != nil {
		t.Fatalf("back to free: %v", err)
	}
	if rec.BasePlanSlug != "" {
		t.Errorf("base plan on free = %q, want none", rec.BasePlanSlug)
	}
}

// `--events 0` clears an override, which on a deal falls back to its plan's
// allowance — the opposite of "bill from the first event", the natural reading.
func TestADealsAllowanceIsNotClearedToTheDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"), IncludedEvents: new(int64(0)),
	}); !errors.Is(err, entitlement.ErrDealAllowanceZero) {
		t.Errorf("--events 0 on a deal: err = %v, want ErrDealAllowanceZero", err)
	}
	// A comp may clear its override: free's default is what it falls back to.
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugFree, IncludedEvents: new(int64(0)),
	}); err != nil {
		t.Errorf("--events 0 on free: %v", err)
	}
}

// The transitions an operator makes on a deal, each from a stored one: a renewal
// that ends the term early, winding it down to a comp, and dropping its product.
func TestSetPlanDealTransitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	until := time.Now().UTC().AddDate(0, 6, 0).Truncate(time.Hour)
	stage := func(t *testing.T, f *fixture) {
		t.Helper()
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
			PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"),
			IncludedEvents: new(int64(5_000_000)), DisplayName: new("Acme"), ContractEndsAt: &until,
		}); err != nil {
			t.Fatalf("stage deal: %v", err)
		}
	}

	t.Run("an empty --until ends only the term", func(t *testing.T) {
		f := newFixture(t)
		stage(t, f)
		rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
			PlanSlug: entitlement.SlugCustom, ContractEndsAt: &time.Time{},
		})
		if err != nil {
			t.Fatalf("SetPlan: %v", err)
		}
		if !rec.ContractEndsAt.IsZero() || rec.IncludedEventsOverride != 5_000_000 ||
			rec.DisplayNameOverride != "Acme" || rec.ProviderProductID != "prod_deal" {
			t.Errorf("rec = %+v, want the deal's terms with no contract", rec)
		}
	})

	t.Run("a deal becomes a comp once its product is cleared", func(t *testing.T) {
		f := newFixture(t)
		stage(t, f)
		comp := entitlement.Change{PlanSlug: entitlement.SlugFree, ContractEndsAt: &until}
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, comp); !errors.Is(err, entitlement.ErrProductNeedsCustom) {
			t.Fatalf("a comp keeping the deal's product: err = %v, want ErrProductNeedsCustom", err)
		}
		comp.ProviderProductID = new("")
		rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, comp)
		if err != nil {
			t.Fatalf("SetPlan: %v", err)
		}
		if rec.PlanSlug != entitlement.SlugFree || rec.ProviderProductID != "" || rec.BasePlanSlug != "" ||
			rec.IncludedEventsOverride != 5_000_000 || !rec.ContractEndsAt.Equal(until) {
			t.Errorf("rec = %+v, want a comp keeping the deal's overrides and term", rec)
		}
	})

	t.Run("a deal cannot drop its product", func(t *testing.T) {
		f := newFixture(t)
		stage(t, f)
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
			PlanSlug: entitlement.SlugCustom, ProviderProductID: new(""),
		}); !errors.Is(err, entitlement.ErrCustomNeedsProduct) {
			t.Errorf("err = %v, want ErrCustomNeedsProduct", err)
		}
	})
}

func seedLiveCustomSubscription(t *testing.T, pg *testutil.TestPostgres, orgID string) {
	t.Helper()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, id, org_id, plan_slug, price_cents,
		   provider, provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', now() + interval '20 days', $1, $2, 'custom', 100, 'dodo', 'cus_1', 'active', 'sub_1',
		         now() - interval '1 hour', 'active')`,
		xid.New().String(), orgID); err != nil {
		t.Fatalf("seed live custom subscription: %v", err)
	}
}
