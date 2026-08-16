package billing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func TestAnchorDayOutOfRangeIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	// 65537 truncates to 1 as an int16, so an unchecked write would store a
	// plausible day and report success.
	for _, day := range []int{32, 65537, -1} {
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
			PlanSlug:  "growth",
			AnchorDay: new(day),
		})
		if !errors.Is(err, corebilling.ErrAnchorDayRange) {
			t.Errorf("anchor day %d: err = %v, want ErrAnchorDayRange", day, err)
		}
	}
}

func TestExtendTrialIsRefusedOnAGrantedPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: "growth",
	}); err != nil {
		t.Fatalf("set growth: %v", err)
	}

	// A granted plan resolves ahead of any trial date, so the write would store a
	// date that changes nothing and still print as a success.
	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now())
	if !errors.Is(err, corebilling.ErrTrialOnGrantedPlan) {
		t.Errorf("err = %v, want ErrTrialOnGrantedPlan", err)
	}
}

// A slug the catalog has dropped resolves free without ever consulting a trial
// date, so extending one would store a date that changes nothing — the same
// silent success the guard above exists to prevent.
func TestExtendTrialIsRefusedOnAnUnknownPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	// Past SetPlan, which rejects the slug: only a catalog removal produces this.
	if _, err := f.pg.PgW.Exec(t.Context(),
		"insert into billing_entitlements (org_id, plan_slug) values ($1, 'growth-v0')", f.orgID); err != nil {
		t.Fatalf("seed an unknown slug: %v", err)
	}

	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now())
	if !errors.Is(err, corebilling.ErrTrialOnGrantedPlan) {
		t.Errorf("err = %v, want ErrTrialOnGrantedPlan", err)
	}
}

// "Extend" is measured from now, so a small --days on a trial with longer to run
// would shorten it. The fixture org is long past its derived trial, so this sets
// a live one first.
func TestExtendTrialWillNotShortenOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	now := time.Now()
	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, now); err != nil {
		t.Fatalf("extend to 30 days: %v", err)
	}

	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 3, now)
	if !errors.Is(err, corebilling.ErrTrialNotExtended) {
		t.Errorf("err = %v, want ErrTrialNotExtended; a 3-day extend cut a 30-day trial", err)
	}

	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 400, now); !errors.Is(err, corebilling.ErrTrialDaysRange) {
		t.Errorf("err = %v, want ErrTrialDaysRange", err)
	}
}

// The contract belongs to the granted plan, so a downgrade must not leave a
// future date behind for a dashboard to render as "your Free plan ends...".
func TestDowngradeToAFloorPlanClearsTheContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       "growth",
		ContractEndsAt: new(until),
	}); err != nil {
		t.Fatalf("set growth: %v", err)
	}

	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugFree})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if !rec.ContractEndsAt.IsZero() {
		t.Errorf("contract_ends_at = %s after downgrading to free, want it cleared", rec.ContractEndsAt)
	}

	// A comped pilot names its own end date, and that one is kept.
	pilot, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugFree,
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(until),
	})
	if err != nil {
		t.Fatalf("set comped pilot: %v", err)
	}
	if !pilot.ContractEndsAt.Equal(until) {
		t.Errorf("contract_ends_at = %s, want the pilot's %s", pilot.ContractEndsAt, until)
	}
}

func TestClearDistinguishesAnUnknownOrgFromAnEmptyOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if err := f.svc.Clear(t.Context(), f.orgID, actor); !errors.Is(err, corebilling.ErrNoEntitlement) {
		t.Errorf("clear on an org with no row: err = %v, want ErrNoEntitlement", err)
	}
	if err := f.svc.Clear(t.Context(), "o_doesnotexist00000", actor); !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Errorf("clear on an unknown org: err = %v, want ErrOrgNotFound", err)
	}
}

func TestStoredRecordShowsAnOverrideThatIsNotInForce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	lapsed := time.Now().Add(-time.Hour)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       "scale",
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(lapsed),
		Note:           new("annual wire, INV-123"),
	}); err != nil {
		t.Fatalf("set scale: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != 10_000 {
		t.Errorf("resolved quota = %v, want the free floor after the contract lapsed", got)
	}

	// The override the resolved answer hides is the one that carries onto the next
	// `set`, so `show` has to print it.
	rec, err := f.svc.StoredRecord(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.IncludedEventsOverride != 5_000_000 {
		t.Errorf("stored override = %d, want 5000000", rec.IncludedEventsOverride)
	}
	if rec.Note != "annual wire, INV-123" {
		t.Errorf("stored note = %q, want it readable from the current row", rec.Note)
	}
}

// The atomicity the design leans on: a change and its history row commit together
// or not at all. Forced by an actor longer than history.actor's varchar(150), so
// the entitlement upsert succeeds and only the history insert fails.
func TestAFailedHistoryAppendRollsBackTheChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, strings.Repeat("x", 200), corebilling.Change{
		PlanSlug: "growth",
	})
	if err == nil {
		t.Fatal("SetPlan with an over-long actor: err = nil, want the history insert to fail")
	}

	var rows int
	if err := f.pg.PgRO.QueryRow(t.Context(),
		"select count(*) from billing_entitlements where org_id = $1", f.orgID).Scan(&rows); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d entitlement rows survived a failed history append, want 0", rows)
	}
}

// The mirror of the contract clear: converting a trial to a paid tier must drop
// the stored trial date, or a later downgrade to free resurrects it and the org
// resolves TRIALING on the trial's much larger quota.
func TestConvertingATrialToAPaidPlanClearsTheTrialDate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 90, time.Now()); err != nil {
		t.Fatalf("extend trial: %v", err)
	}

	converted, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: "growth"})
	if err != nil {
		t.Fatalf("convert to growth: %v", err)
	}
	if !converted.TrialEndsAt.IsZero() {
		t.Errorf("trial_ends_at = %s after converting to growth, want it cleared", converted.TrialEndsAt)
	}

	// The date must stay gone through a later downgrade, which is where a stale
	// one would actually bite.
	back, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugFree})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if !back.TrialEndsAt.IsZero() {
		t.Fatalf("trial_ends_at = %s after downgrading, want it cleared", back.TrialEndsAt)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s, want FREE; a stale trial date restored a trial quota", ent.Status)
	}
}

// Clear takes the same org lock mutate does. Without it a concurrent SetPlan can
// insert between the delete and the commit, leaving a stored entitlement whose
// newest history entry says it was cleared.
func TestClearTakesTheOrgLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: "growth"}); err != nil {
		t.Fatalf("set growth: %v", err)
	}

	tx, err := f.pg.PgW.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := dbwrite.New(tx).LockBillingEntitlementOrg(t.Context(), f.orgID); err != nil {
		t.Fatalf("lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- f.svc.Clear(t.Context(), f.orgID, actor) }()

	select {
	case err := <-done:
		t.Fatalf("Clear finished (%v) while the org lock was held; it is not taking the lock", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Clear after the lock was released: %v", err)
	}
}
