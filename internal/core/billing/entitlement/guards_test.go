package entitlement_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestAnchorDayOutOfRangeIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	// 65537 truncates to 1 as an int16, so an unchecked write would store a
	// plausible day and report success.
	for _, day := range []int{32, 65537, -1} {
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
			PlanSlug:  entitlement.SlugFree,
			AnchorDay: new(day),
		})
		if !errors.Is(err, entitlement.ErrAnchorDayRange) {
			t.Errorf("anchor day %d: err = %v, want ErrAnchorDayRange", day, err)
		}
	}
}

// The contract belongs to the deal, so a downgrade must not leave a
// future date behind for a dashboard to render as "your Free plan ends...".
func TestDowngradeToFreeClearsTheContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		ProviderProductID: new("prod_deal"),
		ContractEndsAt:    new(until),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if !rec.ContractEndsAt.IsZero() {
		t.Errorf("contract_ends_at = %s after downgrading to free, want it cleared", rec.ContractEndsAt)
	}

	// A comped pilot names its own end date, and that one is kept.
	pilot, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugFree,
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
	if err := f.svc.Clear(t.Context(), f.orgID, actor); !errors.Is(err, entitlement.ErrNoEntitlement) {
		t.Errorf("clear on an org with no row: err = %v, want ErrNoEntitlement", err)
	}
	if err := f.svc.Clear(t.Context(), "o_doesnotexist00000", actor); !errors.Is(err, entitlement.ErrOrgNotFound) {
		t.Errorf("clear on an unknown org: err = %v, want ErrOrgNotFound", err)
	}
}

func TestStoredRecordShowsAnOverrideThatIsNotInForce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	lapsed := time.Now().Add(-time.Hour)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugFree,
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(lapsed),
		Note:           new("annual wire, INV-123"),
	}); err != nil {
		t.Fatalf("set the comp: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != entitlement.CurrentPlan().FreeEvents {
		t.Errorf("resolved quota = %v, want the current allowance after the comp lapsed", got)
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
	_, err := f.svc.SetPlan(t.Context(), f.orgID, strings.Repeat("x", 200), entitlement.Change{
		PlanSlug: entitlement.SlugFree,
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

// Clear takes the same org lock mutate does. Without it a concurrent SetPlan can
// insert between the delete and the commit, leaving a stored entitlement whose
// newest history entry says it was cleared.
func TestClearTakesTheOrgLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
		t.Fatalf("set free: %v", err)
	}

	tx, err := f.pg.PgW.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := dbwrite.New(tx).LockBillingEntitlementOrg(t.Context(), f.orgID); err != nil {
		t.Fatalf("lock: %v", err)
	}

	var clearErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		clearErr = f.svc.Clear(t.Context(), f.orgID, actor)
	}()

	testutil.WaitForAdvisoryLockWaiter(t, f.pg.PgRO, done)
	select {
	case <-done:
		t.Fatalf("Clear finished (%v) while the org lock was held; it is not taking the lock", clearErr)
	default:
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	<-done
	if clearErr != nil {
		t.Fatalf("Clear after the lock was released: %v", clearErr)
	}
}

// `--until ""` is how an operator ENDS a deal, so it clears the overrides the
// contract gated exactly as omitting the flag does. It reaches the service as a
// non-nil pointer to the zero time — the one input that looks like a date.
func TestClearingTheContractExplicitlyEndsTheOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Now().AddDate(0, 1, 0)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		IncludedEvents:    new(int64(5_000_000)),
		RetentionDays:     new(int64(3650)),
		DisplayName:       new("Acme Enterprise"),
		ContractEndsAt:    new(until),
		ProviderProductID: new("prod_acme"),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	var zero time.Time
	dropped, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugFree,
		ContractEndsAt: &zero,
	})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if dropped.IncludedEventsOverride != 0 || dropped.RetentionDaysOverride != 0 ||
		dropped.DisplayNameOverride != "" || dropped.ProviderProductID != "" {
		t.Errorf("overrides after an explicit --until \"\" = %+v, want them all cleared", dropped)
	}
	if !dropped.ContractEndsAt.IsZero() {
		t.Errorf("contract_ends_at = %v, want it cleared", dropped.ContractEndsAt)
	}
}

// The mirror of the case above: free WITH a date is a comp, and its overrides are
// the whole point of it.
func TestFreeWithAContractKeepsItsOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Now().AddDate(0, 1, 0)

	comped, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugFree,
		IncludedEvents: new(int64(2_000_000)),
		ContractEndsAt: &until,
	})
	if err != nil {
		t.Fatalf("comped grant: %v", err)
	}
	if comped.IncludedEventsOverride != 2_000_000 || comped.ContractEndsAt.IsZero() {
		t.Errorf("comped grant = %+v, want the quota and the date kept", comped)
	}
}

// The contract is what expires an override, so clearing it on a downgrade has to
// take the overrides with it — otherwise the deal a lapse would have ended
// becomes permanent, and "downgrade to free" leaves a larger quota than doing
// nothing at all.
func TestDowngradeToFreeEndsTheOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	now := time.Now()
	until := now.AddDate(0, 1, 0)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:          entitlement.SlugCustom,
		IncludedEvents:    new(int64(5_000_000)),
		DisplayName:       new("Acme Enterprise"),
		ContractEndsAt:    new(until),
		ProviderProductID: new("prod_acme"),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	dropped, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	// Kept, it would go on offering a buy button for the deal that just ended.
	if dropped.ProviderProductID != "" {
		t.Errorf("provider product = %q, want it dropped with the rest", dropped.ProviderProductID)
	}

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, until.AddDate(5, 0, 0))
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != entitlement.CurrentPlan().FreeEvents {
		t.Errorf("quota five years after the downgrade = %v, want the current allowance", got)
	}
	if ent.DisplayName != "Free" {
		t.Errorf("display name = %q, want Free, not the deal's", ent.DisplayName)
	}

	// A comp on free names its own terms, and those survive.
	pilot, err := f.svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{
		PlanSlug:       entitlement.SlugFree,
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(until),
	})
	if err != nil {
		t.Fatalf("set comped pilot: %v", err)
	}
	if pilot.IncludedEventsOverride != 5_000_000 {
		t.Errorf("pilot quota = %d, want the named 5000000", pilot.IncludedEventsOverride)
	}
}

// An operator's display name is free text, and varchar(150) would otherwise
// surface as a raw SQLSTATE logged as a pug fault.
func TestOverLongDisplayNameIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:    entitlement.SlugFree,
		DisplayName: new(strings.Repeat("x", entitlement.MaxDisplayNameLen+1)),
	})
	if !errors.Is(err, entitlement.ErrDisplayNameLong) {
		t.Errorf("err = %v, want ErrDisplayNameLong", err)
	}

	// varchar(150) bounds characters, so a multi-byte name at the limit fits.
	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug:    entitlement.SlugFree,
		DisplayName: new(strings.Repeat("\u00e9", entitlement.MaxDisplayNameLen)),
	})
	if err != nil {
		t.Fatalf("set plan with a 150-character non-ASCII name: %v", err)
	}
	if got := utf8.RuneCountInString(rec.DisplayNameOverride); got != entitlement.MaxDisplayNameLen {
		t.Errorf("stored name = %d characters, want %d", got, entitlement.MaxDisplayNameLen)
	}
}

// The lock mutate takes before its read, which is the one `for update` cannot
// stand in for: it locks nothing when the org has no row yet, so without this two
// concurrent first writes both read an empty record and the second wins.
func TestSetPlanTakesTheOrgLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	tx, err := f.pg.PgW.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := dbwrite.New(tx).LockBillingEntitlementOrg(t.Context(), f.orgID); err != nil {
		t.Fatalf("lock: %v", err)
	}

	var setErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, setErr = f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree})
	}()

	testutil.WaitForAdvisoryLockWaiter(t, f.pg.PgRO, done)
	select {
	case <-done:
		t.Fatalf("SetPlan finished (%v) while the org lock was held; it is not taking the lock", setErr)
	default:
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	<-done
	if setErr != nil {
		t.Fatalf("SetPlan after the lock was released: %v", setErr)
	}
}

// The lock must come before the read. `for update` locks nothing while the org has
// no row, so a read taken first would see an operator's uncommitted first grant as
// no row at all, and a subscription writer would map a paid deal against that —
// consuming the delivery and leaving the org on free.
func TestWithOrgLockReadsOnlyOnceItHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	// An operator's first grant, mid-flight: the lock held and the row written but
	// not yet committed.
	grant, err := f.pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = grant.Rollback(ctx) }()
	if err := dbwrite.New(grant).LockBillingEntitlementOrg(ctx, f.orgID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := grant.Exec(ctx,
		`insert into billing_entitlements (org_id, plan_slug, included_events_override, provider_product_id)
		 values ($1, 'custom', 5000000, 'prod_acme')`, f.orgID); err != nil {
		t.Fatalf("stage the grant: %v", err)
	}

	var got entitlement.Record
	var lockErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		lockErr = f.svc.WithOrgLock(ctx, f.orgID, func(_ *dbwrite.Queries, cur entitlement.Record) error {
			got = cur
			return nil
		})
	}()

	testutil.WaitForAdvisoryLockWaiter(t, f.pg.PgRO, done)
	if err := grant.Commit(ctx); err != nil {
		t.Fatalf("commit the grant: %v", err)
	}
	<-done
	if lockErr != nil {
		t.Fatalf("WithOrgLock: %v", lockErr)
	}
	if !got.Present || got.ProviderProductID != "prod_acme" {
		t.Errorf("WithOrgLock handed over %+v, want the committed grant: it read before it held the lock", got)
	}
}

// No row is the ordinary state, and an org that does not exist reads the same way:
// the lock is advisory, so taking it proves nothing about the org.
func TestWithOrgLockHandsOverNoRowAsTheZeroRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for name, orgID := range map[string]string{"no row": f.orgID, "no such org": "o_does_not_exist"} {
		t.Run(name, func(t *testing.T) {
			called := false
			err := f.svc.WithOrgLock(t.Context(), orgID, func(_ *dbwrite.Queries, cur entitlement.Record) error {
				called = true
				if cur.Present || cur.PlanSlug != "" {
					t.Errorf("record = %+v, want the zero Record", cur)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("WithOrgLock: %v", err)
			}
			if !called {
				t.Error("WithOrgLock returned without running fn")
			}
		})
	}
}

// fn's error is the rollback: a caller that refuses halfway must leave nothing
// behind, or a refused delivery would still have written.
func TestWithOrgLockCommitsOnlyWhenFnSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	refused := errors.New("refused after writing")
	err := f.svc.WithOrgLock(t.Context(), f.orgID, func(w *dbwrite.Queries, _ entitlement.Record) error {
		if _, err := w.UpsertBillingEntitlement(t.Context(), dbwrite.UpsertBillingEntitlementParams{
			OrgID:    f.orgID,
			PlanSlug: entitlement.SlugFree,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want fn's own error back", err)
	}
	rec, err := f.svc.StoredRecord(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.Present {
		t.Errorf("stored %+v; the write fn refused was committed anyway", rec)
	}
}

// A refused fn must let go of the lock as well as the write. The write is
// invisible either way, so only taking the lock again shows a transaction left
// open, and in production every later write for the org would queue behind it: a
// delivery, a confirm, `billing set`.
func TestWithOrgLockReleasesTheLockWhenFnFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	// A pool of its own, closed with a deadline: a leaked transaction never returns
	// its connection, and closing the harness's pool would wait on it until the
	// test binary timed out.
	pool, err := pgxpool.NewWithConfig(t.Context(), f.pg.PgW.Config())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { pool.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Error("a connection never came back to the pool: WithOrgLock left a transaction open")
		}
	})
	svc, err := entitlement.NewService(f.pg.PgRO, pool, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	refused := errors.New("refused")
	err = svc.WithOrgLock(t.Context(), f.orgID, func(*dbwrite.Queries, entitlement.Record) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want fn's own error back", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := svc.WithOrgLock(ctx, f.orgID, func(*dbwrite.Queries, entitlement.Record) error { return nil }); err != nil {
		t.Fatalf("taking the org lock after fn failed: %v; the refused call still holds it", err)
	}
}

// The guard runs inside mutate's transaction, which already holds a connection.
// Off the pool it waits on that connection and only stops at the deadline.
func TestSetPlanGuardTakesNoSecondConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	cfg := f.pg.PgW.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	defer pool.Close()

	// A staged deal, so leaving it runs the guard's read inside the tx.
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"),
	}); err != nil {
		t.Fatalf("stage the deal: %v", err)
	}
	svc, err := entitlement.NewService(f.pg.PgRO, pool, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := svc.SetPlan(ctx, f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
		t.Fatalf("SetPlan on a one-connection pool: %v", err)
	}
}

// The columns' `> 0` checks, mirrored so an operator sees a named error rather
// than a raw SQLSTATE.
func TestNegativeOverridesAreRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	negative := int64(-1)

	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugFree, IncludedEvents: &negative,
	})
	if !errors.Is(err, entitlement.ErrQuotaNegative) {
		t.Errorf("err = %v, want ErrQuotaNegative", err)
	}
	_, err = f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugFree, RetentionDays: &negative,
	})
	if !errors.Is(err, entitlement.ErrRetentionNegative) {
		t.Errorf("err = %v, want ErrRetentionNegative", err)
	}
}

// Rows outlive a slug dropped from the Go catalog, so failing the read would take
// the dashboard down for whoever holds it. The row's slug decides nothing now: the
// org is free on the current allowance.
func TestAnEntitlementNamingAnUnknownPlanStillReads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	// Straight to the column: no writer would store this.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set plan_slug = 'growth-v9' where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("rewrite the slug: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement on a slug the catalog dropped: %v", err)
	}
	if ent.Status != entitlement.StatusFree || ent.IncludedEvents == nil ||
		*ent.IncludedEvents != entitlement.CurrentPlan().FreeEvents {
		t.Errorf("resolved %s with %v, want FREE on the current allowance", ent.Status, ent.IncludedEvents)
	}
}
