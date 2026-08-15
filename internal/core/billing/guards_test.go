package billing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func TestClearingAPriceClearsItsCurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:   "growth",
		PriceCents: corebilling.Set(int64(40_000)),
		Currency:   corebilling.Set("EUR"),
	}); err != nil {
		t.Fatalf("set price: %v", err)
	}

	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:   "growth",
		PriceCents: corebilling.Clear[int64](),
	})
	if err != nil {
		t.Fatalf("clear price: %v", err)
	}
	if rec.PriceCentsOverride != nil {
		t.Errorf("price = %d after clearing, want none", *rec.PriceCentsOverride)
	}
	if rec.CurrencyOverride != "" {
		t.Errorf("currency = %q after clearing the price, want it cleared too", rec.CurrencyOverride)
	}
}

func TestCurrencyWithoutAPriceIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: "growth",
		Currency: corebilling.Set("EUR"),
	})
	if !errors.Is(err, corebilling.ErrCurrencyNeedsPrice) {
		t.Errorf("err = %v, want ErrCurrencyNeedsPrice rather than a raw constraint violation", err)
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
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
			PlanSlug:  "growth",
			AnchorDay: corebilling.Set(day),
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

// char(3) blank-pads anything shorter and rejects anything longer as a raw
// SQLSTATE, and this flag is the only way a non-USD amount enters pug.
func TestCurrencyMustBeAThreeLetterCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	for _, code := range []string{"EU", "USDX", "US1"} {
		_, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
			PlanSlug:   "growth",
			PriceCents: corebilling.Set(int64(40_000)),
			Currency:   corebilling.Set(code),
		})
		if !errors.Is(err, corebilling.ErrCurrencyInvalid) {
			t.Errorf("currency %q: err = %v, want ErrCurrencyInvalid", code, err)
		}
	}

	rec, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:   "growth",
		PriceCents: corebilling.Set(int64(40_000)),
		Currency:   corebilling.Set("eur"),
	})
	if err != nil {
		t.Fatalf("set eur: %v", err)
	}
	if rec.CurrencyOverride != "EUR" {
		t.Errorf("currency = %q, want it normalized to EUR", rec.CurrencyOverride)
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
		ContractEndsAt: corebilling.Set(until),
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
		IncludedEvents: corebilling.Set(int64(5_000_000)),
		ContractEndsAt: corebilling.Set(until),
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
		IncludedEvents: corebilling.Set(int64(5_000_000)),
		ContractEndsAt: corebilling.Set(lapsed),
		Note:           corebilling.Set("annual wire, INV-123"),
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
