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

// Orgs are backdated well past the trial so a test asserting a deal is not also
// fighting a live trial window.
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

	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, nil)
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
	if ent.Status != corebilling.StatusFree || ent.Slug != currentCard().Slug {
		t.Errorf("status/slug = %s/%s, want FREE on the current card for an org past its trial",
			ent.Status, ent.Slug)
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
			change := pinCard()
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

	change := pinCard()
	change.ContractEndsAt = &until
	change.Note = new("annual wire, INV-123")
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, change); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	card := currentCard()
	if ent.Status != corebilling.StatusFree || ent.Slug != card.Slug {
		t.Errorf("status/slug = %s/%s, want FREE on %s — a pin prices usage, it does not grant",
			ent.Status, ent.Slug, card.Slug)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != card.FreeEvents {
		t.Errorf("allowance = %v, want the card's %d", ent.IncludedEvents, card.FreeEvents)
	}
	if !ent.ContractEndsAt.Equal(until) {
		t.Errorf("contract_ends_at = %s, want %s", ent.ContractEndsAt, until)
	}
}

// A deal's retention is stored beside its money and resolves the same way, which
// is the whole reason it is a column rather than prose in the note.
func TestNegotiatedRetentionRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	deal := setDeal()
	deal.RetentionDays = new(int64(10 * corebilling.RetentionYearDays))
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, deal); err != nil {
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
// renewal, and reverting a negotiated rate to the card's would be silent.
func TestReSetKeepsUnmentionedOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	deal := setDeal()
	deal.RateCentsPerMillion = new(int64(3_000))
	deal.IncludedEvents = new(int64(5_000_000))
	deal.RetentionDays = new(int64(3_650))
	deal.DisplayName = new("Acme Enterprise")
	deal.AnchorDay = new(17)
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, deal); err != nil {
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
	if rec.FlatFeeCents != 40_000 || rec.RateCentsPerMillion != 3_000 {
		t.Errorf("money after a renewal = %d/%d, want the negotiated terms preserved",
			rec.FlatFeeCents, rec.RateCentsPerMillion)
	}
	if rec.IncludedEventsOverride != 5_000_000 {
		t.Errorf("allowance after a renewal = %d, want the negotiated 5000000 preserved", rec.IncludedEventsOverride)
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
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		IncludedEvents: new(int64),
		RetentionDays:  new(int64),
		DisplayName:    new(string),
	})
	if err != nil {
		t.Fatalf("clearing SetPlan: %v", err)
	}
	if cleared.IncludedEventsOverride != 0 || cleared.DisplayNameOverride != "" || cleared.RetentionDaysOverride != 0 {
		t.Errorf("cleared overrides = %d/%q/%d, want empty",
			cleared.IncludedEventsOverride, cleared.DisplayNameOverride, cleared.RetentionDaysOverride)
	}
}

// The database is the guard, not the CLI: the row is what every read trusts.
func TestCustomPlanRequiresAPrice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugCustom})
	if !errors.Is(err, corebilling.ErrCustomNeedsPrice) {
		t.Fatalf("err = %v, want ErrCustomNeedsPrice", err)
	}

	// Straight past the service, to prove the constraint itself holds. A zero is
	// checked as well as a NULL, or a hand-written 0 makes a free tier of a deal.
	for _, money := range []string{"null, null", "0, 0"} {
		_, err = f.pg.PgW.Exec(t.Context(),
			`insert into billing_entitlements (org_id, plan_slug, flat_fee_cents, rate_cents_per_million)
			 values ($1, 'custom', `+money+`)`, f.orgID)
		var pgErr *pgconn.PgError
		if err == nil {
			t.Errorf("the database accepted a custom entitlement with %s", money)
		} else if !errors.As(err, &pgErr) || pgErr.ConstraintName != "billing_entitlements_custom_needs_price" {
			t.Errorf("err = %v, want the custom_needs_price constraint", err)
		}
	}
}

// Every slug the Go catalog knows must be storable. There is deliberately no
// plan_slug check constraint, so this is what catches a slug outgrowing
// varchar(50) or being rejected by the service.
func TestEveryCardSlugIsStorable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, card := range corebilling.Cards() {
		if card.Retired {
			// One org holds every slug in turn, and a retired card is settable only by
			// the org already on it. retired_test.go stores one on its incumbent.
			continue
		}
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: card.Slug}); err != nil {
			t.Errorf("%s: %v — this card slug is not storable", card.Slug, err)
		}
	}
	// And the two that are not cards: a deal, and no pin at all.
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, setDeal()); err != nil {
		t.Errorf("custom: %v", err)
	}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: ""}); err != nil {
		t.Errorf("the empty slug: %v", err)
	}
}

// An operator must be able to record a deal for an org that has never held one —
// that is what a negotiated deal IS.
func TestCustomPlanIsGrantableToAnyOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	deal := setDeal()
	deal.RateCentsPerMillion = new(int64(3_000))
	deal.IncludedEvents = new(int64(5_000_000))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, deal); err != nil {
		t.Fatalf("recording a custom deal: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Terms == nil || ent.Terms.RateCentsPerMillion != 3_000 {
		t.Errorf("terms = %v, want the negotiated rate", ent.Terms)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != 5_000_000 {
		t.Errorf("allowance = %v, want the negotiated 5000000", ent.IncludedEvents)
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
	// Extending a trial pins nothing: the row carries a date and no plan.
	rec, err := f.svc.StoredRecord(ctx, f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.PlanSlug != "" {
		t.Errorf("plan slug = %q after extend-trial, want none stored", rec.PlanSlug)
	}

	if err := f.svc.Clear(ctx, f.orgID, actor); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	ent, err = f.svc.GetEntitlement(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("GetEntitlement after clear: %v", err)
	}
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s after clear, want FREE (the org is back on its age)", ent.Status)
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

	deal := setDeal()
	deal.RateCentsPerMillion = new(int64(3_000))
	deal.IncludedEvents = new(int64(5_000_000))
	deal.RetentionDays = new(int64(2555))
	deal.DisplayName = new("Acme Enterprise")
	deal.AnchorDay = new(11)
	deal.ContractEndsAt = &until
	deal.Note = new("$400/mo, INV-123")
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, deal); err != nil {
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
	if got.PlanSlug != corebilling.SlugCustom || got.FlatFeeCents != 40_000 ||
		got.RateCentsPerMillion != 3_000 || got.IncludedEventsOverride != 5_000_000 ||
		got.RetentionDaysOverride != 2555 || got.DisplayNameOverride != "Acme Enterprise" ||
		got.AnchorDay != 11 || got.Note != "$400/mo, INV-123" || !got.ContractEndsAt.Equal(until) {
		t.Errorf("history record = %+v, want every term the deal named", got)
	}
}

func TestHistoryRecordsEveryChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	pin := pinCard()
	pin.Note = new("first")
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, pin); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	deal := setDeal()
	deal.Note = new("upgrade")
	if _, err := f.svc.SetPlan(ctx, f.orgID, "someone@else", deal); err != nil {
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
		t.Fatalf("history has %d entries, want 3 (two writes and a clear)", len(entries))
	}
	// Newest first, and the clear is a snapshot with no values. A NULL plan_slug
	// no longer means one, so the marker is its own column.
	if entries[0].Record.Present {
		t.Errorf("the newest entry has values; a clear must record an empty snapshot")
	}
	if entries[1].Record.PlanSlug != corebilling.SlugCustom || entries[1].Actor != "someone@else" {
		t.Errorf("entry 1 = %s by %s, want custom by someone@else",
			entries[1].Record.PlanSlug, entries[1].Actor)
	}
	if entries[2].Record.PlanSlug != currentCard().Slug || entries[2].Record.Note != "first" {
		t.Errorf("entry 2 = %s/%q, want the card/first", entries[2].Record.PlanSlug, entries[2].Record.Note)
	}
}

// A row that carries a trial end and no pin is a real state now, and it must not
// read back as a deletion.
func TestHistoryTellsANullPinFromADeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.svc.ExtendTrial(ctx, f.orgID, actor, 30, time.Now()); err != nil {
		t.Fatalf("ExtendTrial: %v", err)
	}
	if err := f.svc.Clear(ctx, f.orgID, actor); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	entries, err := f.svc.History(ctx, f.orgID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history has %d entries, want 2", len(entries))
	}
	if entries[0].Record.Present {
		t.Error("the clear did not record a deletion")
	}
	if !entries[1].Record.Present {
		t.Error("a trial extension with no pin read back as a deletion")
	}
	if entries[1].Record.TrialEndsAt.IsZero() {
		t.Error("the trial extension recorded no date")
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
		t.Fatal("a custom plan with no money was accepted")
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
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, blank, pinCard()); !errors.Is(err, corebilling.ErrActorRequired) {
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
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, setDeal()); err != nil {
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
	if len(entries) != 1 || entries[0].Record.PlanSlug != corebilling.SlugCustom {
		t.Errorf("history after deletion = %+v, want the deal preserved", entries)
	}
}

func TestSetPlanReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), xid.New().String(), actor, pinCard())
	if !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Errorf("err = %v, want ErrOrgNotFound", err)
	}
}

// free and trial stopped being plans, so both are now slugs no card answers to.
func TestSetPlanRefusesASlugNoCardAnswersTo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, slug := range []string{corebilling.SlugFree, "trial", "growth", "usage-2019-01-1"} {
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor,
			corebilling.Change{PlanSlug: slug}); !errors.Is(err, corebilling.ErrPlanNotFound) {
			t.Errorf("SetPlan(%q) = %v, want ErrPlanNotFound", slug, err)
		}
	}
}
