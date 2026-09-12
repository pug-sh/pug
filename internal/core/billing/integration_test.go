package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &fixture{
		svc:   svc,
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
	if ent.Status != corebilling.StatusFree || ent.Slug != corebilling.CurrentSlug {
		t.Errorf("status/slug = %s/%s, want FREE on the current card for an org past its trial", ent.Status, ent.Slug)
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
			change := corebilling.Change{PlanSlug: corebilling.CurrentSlug}
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
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		ContractEndsAt: new(until),
		Note:           new("annual wire, INV-123"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusActive || ent.Slug != corebilling.SlugCustom {
		t.Errorf("status/slug = %s/%s, want ACTIVE/custom", ent.Status, ent.Slug)
	}
	if ent.Terms == nil || ent.Terms.FlatFeeCents != 40_000 {
		t.Errorf("terms = %v, want the deal's flat fee", ent.Terms)
	}
	if !ent.ContractEndsAt.Equal(until) {
		t.Errorf("contract_ends_at = %s, want %s", ent.ContractEndsAt, until)
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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
		RetentionDays:  new(int64(10 * corebilling.RetentionYearDays)),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
		RetentionDays:  new(int64(3_650)),
		DisplayName:    new("Acme Enterprise"),
		AnchorDay:      new(17),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	// A renewal: a new end date and nothing else.
	rec, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		ContractEndsAt: new(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("renewal SetPlan: %v", err)
	}
	if rec.FlatFeeCents != 40_000 || rec.BlockRateCents != 300 {
		t.Errorf("money after a renewal = %d/%d, want the negotiated 40000/300 preserved", rec.FlatFeeCents, rec.BlockRateCents)
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

	// And an explicit clear really clears.
	cleared, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.CurrentSlug,
		IncludedEvents: new(int64),
		RetentionDays:  new(int64),
		DisplayName:    new(string),
		FlatFeeCents:   new(int64),
		BlockRateCents: new(int64),
	})
	if err != nil {
		t.Fatalf("clearing SetPlan: %v", err)
	}
	if cleared.IncludedEventsOverride != 0 || cleared.DisplayNameOverride != "" || cleared.RetentionDaysOverride != 0 ||
		cleared.FlatFeeCents != 0 || cleared.BlockRateCents != 0 {
		t.Errorf("cleared overrides = %+v, want empty", cleared)
	}
}

// A deal needs a price: an allowance alone is a free tier nobody agreed to. The
// database is the guard, not the CLI.
func TestCustomPlanRequiresAPrice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, IncludedEvents: new(int64(5_000_000)),
	})
	if !errors.Is(err, corebilling.ErrCustomNeedsPrice) {
		t.Fatalf("err = %v, want ErrCustomNeedsPrice", err)
	}

	// Straight past the service, to prove the constraint itself holds.
	_, err = f.pg.PgW.Exec(t.Context(),
		"insert into billing_entitlements (org_id, plan_slug, included_events_override) values ($1, 'custom', 5000000)", f.orgID)
	var pgErr *pgconn.PgError
	if err == nil {
		t.Error("the database accepted a custom entitlement with no price")
	} else if !errors.As(err, &pgErr) || pgErr.ConstraintName != "billing_entitlements_custom_needs_price" {
		t.Errorf("err = %v, want the custom_needs_price constraint", err)
	}
}

// The allowance prices as whole blocks, so a partial one is refused rather than
// silently rounded.
func TestAllowanceMustBeWholeBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, BlockRateCents: new(int64(300)), IncludedEvents: new(int64(250_000)),
	})
	if !errors.Is(err, corebilling.ErrAllowanceNotWholeBlocks) {
		t.Fatalf("err = %v, want ErrAllowanceNotWholeBlocks", err)
	}
	_, err = f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(-1)),
	})
	if !errors.Is(err, corebilling.ErrPriceNegative) {
		t.Fatalf("err = %v, want ErrPriceNegative", err)
	}
}

// Every slug the Go catalog knows must be storable. There is deliberately no
// plan_slug check constraint, so this is what catches a slug outgrowing
// varchar(50) or being rejected by the service.
func TestEveryCatalogSlugIsStorable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	slugs := []string{corebilling.SlugFree}
	for _, card := range corebilling.Cards() {
		if !card.Retired {
			slugs = append(slugs, card.Slug)
		}
	}
	for _, slug := range slugs {
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: slug}); err != nil {
			t.Errorf("%s: %v — this catalog slug is not storable", slug, err)
		}
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(1)),
	}); err != nil {
		t.Errorf("custom: %v — the custom slug is not storable", err)
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugTrial}); !errors.Is(err, corebilling.ErrTrialNotSettable) {
		t.Errorf("trial: err = %v, want ErrTrialNotSettable", err)
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
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
		RetentionDays:  new(int64(2555)),
		DisplayName:    new("Acme Enterprise"),
		AnchorDay:      new(11),
		ContractEndsAt: &until,
		Note:           new("$400/mo, INV-123"),
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
	if got.PlanSlug != corebilling.SlugCustom || got.IncludedEventsOverride != 5_000_000 ||
		got.RetentionDaysOverride != 2555 || got.DisplayNameOverride != "Acme Enterprise" ||
		got.AnchorDay != 11 || got.FlatFeeCents != 40_000 || got.BlockRateCents != 300 ||
		got.Note != "$400/mo, INV-123" || !got.ContractEndsAt.Equal(until) {
		t.Errorf("history record = %+v, want every override the grant named", got)
	}
}

func TestHistoryRecordsEveryChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.CurrentSlug, Note: new("first"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := f.svc.SetPlan(ctx, f.orgID, "someone@else", corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(40_000)), Note: new("upgrade"),
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
	if entries[1].Record.PlanSlug != corebilling.SlugCustom || entries[1].Actor != "someone@else" {
		t.Errorf("entry 1 = %s by %s, want custom by someone@else",
			entries[1].Record.PlanSlug, entries[1].Actor)
	}
	if entries[2].Record.PlanSlug != corebilling.CurrentSlug || entries[2].Record.Note != "first" {
		t.Errorf("entry 2 = %s/%q, want the card/first", entries[2].Record.PlanSlug, entries[2].Record.Note)
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
		t.Fatal("a custom plan with no price was accepted")
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
			corebilling.Change{PlanSlug: corebilling.CurrentSlug}); !errors.Is(err, corebilling.ErrActorRequired) {
			t.Errorf("SetPlan(%q) = %v, want ErrActorRequired", blank, err)
		}
		if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, blank, 30, time.Now()); !errors.Is(err, corebilling.ErrActorRequired) {
			t.Errorf("ExtendTrial(%q) = %v, want ErrActorRequired", blank, err)
		}
		if err := f.svc.Clear(t.Context(), f.orgID, blank); !errors.Is(err, corebilling.ErrActorRequired) {
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
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug}); err != nil {
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
	if len(entries) != 1 || entries[0].Record.PlanSlug != corebilling.CurrentSlug {
		t.Errorf("history after deletion = %+v, want the pin preserved", entries)
	}
}

func TestSetPlanReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), xid.New().String(), actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug})
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
