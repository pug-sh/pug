package billing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
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
			PlanSlug:  corebilling.CurrentSlug,
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
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(40_000)),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	// A deal resolves ahead of any trial date, so the write would store a date
	// that changes nothing and still print as a success.
	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now())
	if !errors.Is(err, corebilling.ErrTrialOnGrantedPlan) {
		t.Errorf("err = %v, want ErrTrialOnGrantedPlan", err)
	}
}

// A pinned card is not a grant: the trial still runs on it, so extending one is
// a real change.
func TestExtendTrialIsAllowedOnAPinnedCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug}); err != nil {
		t.Fatalf("pin the card: %v", err)
	}
	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now()); err != nil {
		t.Fatalf("ExtendTrial on a pinned card: %v", err)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusTrialing || ent.Slug != corebilling.CurrentSlug {
		t.Errorf("status/slug = %s/%s, want TRIALING on the pinned card", ent.Status, ent.Slug)
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
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		ContractEndsAt: new(until),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
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
		PlanSlug:       corebilling.SlugCustom,
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(lapsed),
		Note:           new("annual wire, INV-123"),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != 100_000 {
		t.Errorf("resolved quota = %v, want the current card's allowance after the contract lapsed", got)
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
		PlanSlug: corebilling.CurrentSlug,
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

	converted, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(40_000)),
	})
	if err != nil {
		t.Fatalf("convert to a deal: %v", err)
	}
	if !converted.TrialEndsAt.IsZero() {
		t.Errorf("trial_ends_at = %s after converting to a deal, want it cleared", converted.TrialEndsAt)
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
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug}); err != nil {
		t.Fatalf("pin the card: %v", err)
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

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		IncludedEvents: new(int64(5_000_000)),
		RetentionDays:  new(int64(3650)),
		DisplayName:    new("Acme Enterprise"),
		ContractEndsAt: new(until),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	var zero time.Time
	dropped, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugFree,
		ContractEndsAt: &zero,
	})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if dropped.IncludedEventsOverride != 0 || dropped.RetentionDaysOverride != 0 ||
		dropped.DisplayNameOverride != "" || dropped.FlatFeeCents != 0 {
		t.Errorf("overrides after an explicit --until \"\" = %+v, want them all cleared", dropped)
	}
	if !dropped.ContractEndsAt.IsZero() {
		t.Errorf("contract_ends_at = %v, want it cleared", dropped.ContractEndsAt)
	}
}

// The mirror of the case above: a floor plan WITH a date is a comped grant, and
// its overrides are the whole point of it.
func TestAFloorPlanWithAContractKeepsItsOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Now().AddDate(0, 1, 0)

	comped, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugFree,
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
func TestDowngradeToAFloorPlanEndsTheOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	now := time.Now()
	until := now.AddDate(0, 1, 0)

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		IncludedEvents: new(int64(5_000_000)),
		DisplayName:    new("Acme Enterprise"),
		ContractEndsAt: new(until),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	dropped, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{PlanSlug: corebilling.SlugFree})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	// Kept, the fee would be the next renewal's silent default.
	if dropped.FlatFeeCents != 0 {
		t.Errorf("flat fee = %d, want it dropped with the rest", dropped.FlatFeeCents)
	}

	ent, err := f.svc.GetEntitlement(ctx, f.orgID, until.AddDate(5, 0, 0))
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != 100_000 {
		t.Errorf("quota five years after the downgrade = %v, want the current card's 100000", got)
	}
	if ent.DisplayName != "Usage" {
		t.Errorf("display name = %q, want the card's, not the deal's", ent.DisplayName)
	}

	// A comped grant on the floor names its own terms, and those survive.
	pilot, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugFree,
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

// The other way to store a trial date that resolves to nothing: the trial branch
// is gated on the contract, so a lapsed one swallows the extension exactly as a
// granted plan would.
func TestExtendTrialIsRefusedOnALapsedContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	now := time.Now()
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugFree,
		IncludedEvents: new(int64(5_000_000)),
		ContractEndsAt: new(now.Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("set an ended pilot: %v", err)
	}

	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, now)
	if !errors.Is(err, corebilling.ErrTrialOnLapsedContract) {
		t.Errorf("err = %v, want ErrTrialOnLapsedContract", err)
	}
}

// An operator's display name is free text, and varchar(150) would otherwise
// surface as a raw SQLSTATE logged as a pug fault.
func TestOverLongDisplayNameIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:    corebilling.CurrentSlug,
		DisplayName: new(strings.Repeat("x", corebilling.MaxDisplayNameLen+1)),
	})
	if !errors.Is(err, corebilling.ErrDisplayNameLong) {
		t.Errorf("err = %v, want ErrDisplayNameLong", err)
	}

	// varchar(150) bounds characters, so a multi-byte name at the limit fits.
	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:    corebilling.CurrentSlug,
		DisplayName: new(strings.Repeat("\u00e9", corebilling.MaxDisplayNameLen)),
	})
	if err != nil {
		t.Fatalf("set plan with a 150-character non-ASCII name: %v", err)
	}
	if got := utf8.RuneCountInString(rec.DisplayNameOverride); got != corebilling.MaxDisplayNameLen {
		t.Errorf("stored name = %d characters, want %d", got, corebilling.MaxDisplayNameLen)
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

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("SetPlan finished (%v) while the org lock was held; it is not taking the lock", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("SetPlan after the lock was released: %v", err)
	}
}

// Every read a mutation makes runs inside its own transaction, which already
// holds a connection. Off the pool it would wait on that connection forever.
func TestSetPlanTakesNoSecondConnection(t *testing.T) {
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

	svc, err := corebilling.NewService(f.pg.PgRO, pool, corebilling.Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug}); err != nil {
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

	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.CurrentSlug, IncludedEvents: &negative,
	})
	if !errors.Is(err, corebilling.ErrQuotaNegative) {
		t.Errorf("err = %v, want ErrQuotaNegative", err)
	}
	_, err = f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.CurrentSlug, RetentionDays: &negative,
	})
	if !errors.Is(err, corebilling.ErrRetentionNegative) {
		t.Errorf("err = %v, want ErrRetentionNegative", err)
	}
}

// Rows outlive a tier dropped from the Go catalog, so failing the read would take
// the dashboard down for whoever holds it.
func TestAnEntitlementNamingAnUnknownPlanStillReads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: corebilling.CurrentSlug}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	// Straight to the column: no writer would store this.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set plan_slug = 'usage-2020-01' where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("rewrite the slug: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement on a slug the catalog dropped: %v", err)
	}
	// Fails open: the current card's allowance would tell a paying customer they are over.
	if ent.IncludedEvents != nil {
		t.Errorf("included_events = %d, want absent", *ent.IncludedEvents)
	}
}
