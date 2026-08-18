package usage_test

import (
	"testing"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestPeriodForClampsAnAnchorBelowOne(t *testing.T) {
	// Unreachable through AnchorDay, which never returns below 1, but PeriodFor is
	// exported: day 0 normalizes into the previous month and used to yield a
	// zero-length window that did not contain now.
	for _, day := range []int{0, -3} {
		start, end := coreusage.PeriodFor(utc(2026, time.March, 14), day)
		if !start.Before(end) {
			t.Errorf("anchor %d: window [%s, %s) is not half-open", day, start, end)
		}
		if start.Day() != 1 {
			t.Errorf("anchor %d: start = %s, want it clamped to the 1st", day, start)
		}
	}
}

func TestEarliestPeriodStartFindsTheOldestWindow(t *testing.T) {
	periods := []coreusage.OrgPeriod{
		{OrgID: "c", Start: utc(2026, time.March, 3)},
		{OrgID: "a", Start: utc(2026, time.February, 17)},
		{OrgID: "b", Start: utc(2026, time.March, 1)},
	}
	if got := coreusage.EarliestPeriodStart(periods); !got.Equal(utc(2026, time.February, 17)) {
		t.Errorf("earliest = %s, want the February window", got)
	}
	// Zero for an empty list is what the cron reads as "no widening".
	if got := coreusage.EarliestPeriodStart(nil); !got.IsZero() {
		t.Errorf("earliest of an empty list = %s, want the zero time", got)
	}
}

// The reason the meter's rescan had to widen: with per-org anchors an org's
// current period can have begun before the current calendar month, and a rescan
// stopping at the month boundary would never re-read the earlier part of it.
func TestOrgPeriodsUsesEachOrgsOwnAnchor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	svc := coreusage.NewService(pg.PgRO, pg.PgW)
	w := dbwrite.New(pg.PgW)

	anchored := seedOrgWithAnchor(t, pg, w, 17)
	onTheFirst := seedOrgWithAnchor(t, pg, w, 1)

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	periods, err := svc.OrgPeriods(t.Context(), now)
	if err != nil {
		t.Fatalf("OrgPeriods: %v", err)
	}

	got := map[string]time.Time{}
	for _, p := range periods {
		got[p.OrgID] = p.Start
	}
	if want := utc(2026, time.February, 17); !got[anchored].Equal(want) {
		t.Errorf("anchored org period start = %s, want %s", got[anchored], want)
	}
	if want := utc(2026, time.March, 1); !got[onTheFirst].Equal(want) {
		t.Errorf("day-1 org period start = %s, want %s", got[onTheFirst], want)
	}

	// What the cron widens its full recompute to: the February start, not March's.
	if want := utc(2026, time.February, 17); !coreusage.EarliestPeriodStart(periods).Equal(want) {
		t.Errorf("earliest = %s, want %s — a rescan stopping at the month boundary "+
			"would silently under-count the anchored org", coreusage.EarliestPeriodStart(periods), want)
	}
}

func seedOrgWithAnchor(t *testing.T, pg *testutil.TestPostgres, w *dbwrite.Queries, anchorDay int) string {
	t.Helper()

	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := pg.PgW.Exec(t.Context(),
		"insert into billing_entitlements (org_id, plan_slug, anchor_day) values ($1, 'growth', $2)",
		org.ID, anchorDay); err != nil {
		t.Fatalf("seed entitlement: %v", err)
	}
	return org.ID
}
