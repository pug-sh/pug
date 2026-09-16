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

// pinCard is the ordinary operator write: a card pin with no deal behind it.
func pinCard() corebilling.Change {
	return corebilling.Change{PlanSlug: currentCard().Slug}
}

// setDeal is the other one: a deal with money on it, which is what the guards
// below actually turn on.
func setDeal() corebilling.Change {
	return corebilling.Change{
		PlanSlug:     corebilling.SlugCustom,
		FlatFeeCents: new(int64(40_000)),
	}
}

func TestAnchorDayOutOfRangeIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	// 65537 truncates to 1 as an int16, so an unchecked write would store a
	// plausible day and report success.
	for _, day := range []int{32, 65537, -1} {
		change := pinCard()
		change.AnchorDay = new(day)
		if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); !errors.Is(err, corebilling.ErrAnchorDayRange) {
			t.Errorf("anchor day %d: err = %v, want ErrAnchorDayRange", day, err)
		}
	}
}

// A deal in force resolves ahead of any trial date, so the write would store a
// date that changes nothing and still print as a success.
func TestExtendTrialIsRefusedOnADeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, setDeal()); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	_, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now())
	if !errors.Is(err, corebilling.ErrTrialOnGrantedPlan) {
		t.Errorf("err = %v, want ErrTrialOnGrantedPlan", err)
	}
}

// A card pin is not a conversion: it prices the usage a trial is not charged for,
// so it must NOT block one. The mirror of the refusal above.
func TestExtendTrialIsAllowedOnACardPin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, pinCard()); err != nil {
		t.Fatalf("pin the card: %v", err)
	}

	rec, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 30, time.Now())
	if err != nil {
		t.Fatalf("ExtendTrial on a card pin: %v", err)
	}
	if rec.TrialEndsAt.IsZero() {
		t.Error("no trial end was stored")
	}
	if rec.PlanSlug != currentCard().Slug {
		t.Errorf("plan slug = %q, want the pin kept", rec.PlanSlug)
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

// `--plan ""` removes the pin and KEEPS the rest of the row: free is no longer a
// plan, so there is no downgrade that clears a deal's terms as a side effect.
// Ending one is `--until ""`, tested below.
func TestRemovingAPinKeepsTheRestOfTheRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	until := time.Now().AddDate(0, 1, 0)
	change := pinCard()
	change.IncludedEvents = new(int64(5_000_000))
	change.DisplayName = new("Acme (comped)")
	change.AnchorDay = new(17)
	change.ContractEndsAt = &until
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
		t.Fatalf("set the comp: %v", err)
	}

	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: ""})
	if err != nil {
		t.Fatalf("remove the pin: %v", err)
	}
	if rec.PlanSlug != "" {
		t.Errorf("plan slug = %q, want it removed", rec.PlanSlug)
	}
	if rec.IncludedEventsOverride != 5_000_000 || rec.DisplayNameOverride != "Acme (comped)" ||
		rec.AnchorDay != 17 || rec.ContractEndsAt.IsZero() {
		t.Errorf("row after removing the pin = %+v, want everything but the pin kept", rec)
	}
}

// Money belongs to a deal, so a pin that is not one cannot leave a price behind
// for the next `--plan custom` to satisfy its guard with.
func TestLeavingCustomClearsTheMoney(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	deal := setDeal()
	deal.RateCentsPerMillion = new(int64(3_000))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, deal); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	for name, change := range map[string]corebilling.Change{
		"a card pin":    pinCard(),
		"no pin at all": {PlanSlug: ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change)
			if err != nil {
				t.Fatalf("SetPlan: %v", err)
			}
			if rec.FlatFeeCents != 0 || rec.RateCentsPerMillion != 0 {
				t.Errorf("money after leaving custom = %d/%d, want both cleared",
					rec.FlatFeeCents, rec.RateCentsPerMillion)
			}
			if !rec.TermsEffectiveAt.IsZero() {
				t.Errorf("terms_effective_at = %s, want it cleared with the money", rec.TermsEffectiveAt)
			}
			// Put the deal back for the next case.
			if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, deal); err != nil {
				t.Fatalf("restore the deal: %v", err)
			}
		})
	}
}

// A deal is invoiced from terms_effective_at, not from the row's birth: the row
// usually predates it. Unchanged terms leave the stamp where it is, or every
// renewal would move the day the deal is billed from.
func TestTermsEffectiveAtStampsOnlyWhenTheMoneyChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	first, err := f.svc.SetPlan(t.Context(), f.orgID, actor, setDeal())
	if err != nil {
		t.Fatalf("set the deal: %v", err)
	}
	if first.TermsEffectiveAt.IsZero() {
		t.Fatal("a new deal stored no terms_effective_at; it has no day to be invoiced from")
	}

	// A renewal on unchanged terms: a new end date and nothing else.
	renewed, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		ContractEndsAt: new(time.Now().AddDate(1, 0, 0)),
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !renewed.TermsEffectiveAt.Equal(first.TermsEffectiveAt) {
		t.Errorf("terms_effective_at moved to %s on an unchanged renewal, want %s",
			renewed.TermsEffectiveAt, first.TermsEffectiveAt)
	}

	// Backdated first: SetPlan stamps to the second and this test does both writes
	// inside one, so without it a real restamp is invisible.
	backdated := first.TermsEffectiveAt.AddDate(0, -1, 0)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set terms_effective_at = $2 where org_id = $1`,
		f.orgID, backdated); err != nil {
		t.Fatalf("backdate the stamp: %v", err)
	}

	// The allowance is priced terms too, so changing it restamps.
	repriced, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		IncludedEvents: new(int64(5_000_000)),
	})
	if err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if !repriced.TermsEffectiveAt.After(backdated) {
		t.Errorf("terms_effective_at = %s after changing the allowance, want it restamped past %s",
			repriced.TermsEffectiveAt, backdated)
	}
}

// An allowance alone is a free tier nobody agreed to.
func TestCustomNeedsAFeeOrARate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		IncludedEvents: new(int64(5_000_000)),
	})
	if !errors.Is(err, corebilling.ErrCustomNeedsPrice) {
		t.Errorf("err = %v, want ErrCustomNeedsPrice", err)
	}

	negative := corebilling.Change{PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(-1))}
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, negative); !errors.Is(err, corebilling.ErrPriceNegative) {
		t.Errorf("err = %v, want ErrPriceNegative", err)
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
	change := pinCard()
	change.IncludedEvents = new(int64(5_000_000))
	change.ContractEndsAt = &lapsed
	change.Note = new("annual wire, INV-123")
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
		t.Fatalf("set the comp: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != currentCard().FreeEvents {
		t.Errorf("resolved allowance = %v, want the card's after the contract lapsed", got)
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
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, strings.Repeat("x", 200), pinCard()); err == nil {
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

// Recording a deal must drop a stored trial date, or a later `--plan ""`
// resurrects it and the org resolves TRIALING on terms it is being charged for.
func TestRecordingADealClearsTheTrialDate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 90, time.Now()); err != nil {
		t.Fatalf("extend trial: %v", err)
	}

	converted, err := f.svc.SetPlan(t.Context(), f.orgID, actor, setDeal())
	if err != nil {
		t.Fatalf("record the deal: %v", err)
	}
	if !converted.TrialEndsAt.IsZero() {
		t.Errorf("trial_ends_at = %s after recording a deal, want it cleared", converted.TrialEndsAt)
	}

	// The date must stay gone once the pin is removed, which is where a stale one
	// would actually bite.
	back, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: ""})
	if err != nil {
		t.Fatalf("remove the pin: %v", err)
	}
	if !back.TrialEndsAt.IsZero() {
		t.Fatalf("trial_ends_at = %s after removing the pin, want it cleared", back.TrialEndsAt)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusFree {
		t.Errorf("status = %s, want FREE; a stale trial date restored a trial", ent.Status)
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
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, pinCard()); err != nil {
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

// `--until ""` is how an operator ENDS a deal. It reaches the service as a
// non-nil pointer to the zero time — the one input that looks like a date — and
// what it ends is the contract, which is what expires the overrides it gated.
func TestClearingTheContractEndsTheDeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	until := time.Now().AddDate(0, 1, 0)

	deal := setDeal()
	deal.RateCentsPerMillion = new(int64(3_000))
	deal.IncludedEvents = new(int64(5_000_000))
	deal.DisplayName = new("Acme Enterprise")
	deal.ContractEndsAt = &until
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, deal); err != nil {
		t.Fatalf("set the deal: %v", err)
	}

	var zero time.Time
	dropped, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		ContractEndsAt: &zero,
	})
	if err != nil {
		t.Fatalf("clear the contract: %v", err)
	}
	if !dropped.ContractEndsAt.IsZero() {
		t.Errorf("contract_ends_at = %v, want it cleared", dropped.ContractEndsAt)
	}
	// Open-ended, not ended: clearing the date removes the bound on the deal, and
	// the deal itself is ended by removing its money.
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Terms == nil {
		t.Error("the deal ended when its contract date was cleared; an open-ended deal runs on")
	}
}

// The other way to store a trial date that resolves to nothing: the trial branch
// is gated on the contract, so a lapsed one swallows the extension.
func TestExtendTrialIsRefusedOnALapsedContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	now := time.Now()
	change := pinCard()
	change.IncludedEvents = new(int64(5_000_000))
	change.ContractEndsAt = new(now.Add(-time.Hour))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
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
	long := pinCard()
	long.DisplayName = new(strings.Repeat("x", corebilling.MaxDisplayNameLen+1))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, long); !errors.Is(err, corebilling.ErrDisplayNameLong) {
		t.Errorf("err = %v, want ErrDisplayNameLong", err)
	}

	// varchar(150) bounds characters, so a multi-byte name at the limit fits.
	atLimit := pinCard()
	atLimit.DisplayName = new(strings.Repeat("é", corebilling.MaxDisplayNameLen))
	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, atLimit)
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
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, pinCard())
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

// Every read SetPlan makes runs inside its own transaction, which already holds a
// connection. Off the pool it would wait on that connection and only stop at the
// deadline.
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

	svc, err := corebilling.NewService(f.pg.PgRO, pool, true, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := svc.SetPlan(ctx, f.orgID, actor, pinCard()); err != nil {
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

	events := pinCard()
	events.IncludedEvents = &negative
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, events); !errors.Is(err, corebilling.ErrQuotaNegative) {
		t.Errorf("err = %v, want ErrQuotaNegative", err)
	}
	retention := pinCard()
	retention.RetentionDays = &negative
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, retention); !errors.Is(err, corebilling.ErrRetentionNegative) {
		t.Errorf("err = %v, want ErrRetentionNegative", err)
	}
}

// Rows outlive a card dropped from the Go catalog, so failing the read would take
// the dashboard down for whoever holds it.
func TestAnEntitlementNamingAnUnknownCardStillReads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, pinCard()); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	// Straight to the column: no writer would store this.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set plan_slug = 'usage-2019-01-1' where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("rewrite the slug: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement on a card the catalog dropped: %v", err)
	}
	// Fails open: pricing it on the current card would charge terms nobody sold.
	if ent.IncludedEvents != nil {
		t.Errorf("included_events = %d, want absent", *ent.IncludedEvents)
	}
}
