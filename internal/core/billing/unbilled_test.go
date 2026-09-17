package billing_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// seedPeriodCounts stores the org's last two metered periods, the one before
// periodStart and the one ending at periodEnd.
func seedPeriodCounts(t *testing.T, f *fixture, previous, last int64) {
	t.Helper()
	for _, p := range []struct {
		start, end time.Time
		events     int64
	}{
		{periodStart.AddDate(0, -1, 0), periodStart, previous},
		{periodStart, periodEnd, last},
	} {
		if _, err := f.pg.PgW.Exec(t.Context(),
			`insert into usage_periods (event_count, org_id, period_end, period_start) values ($1, $2, $3, $4)`,
			p.events, f.orgID, p.end, p.start); err != nil {
			t.Fatalf("seed usage period: %v", err)
		}
	}
}

// Unbilled usage is an org no mandate or deal covered, judged on its latest closed period.
func TestUnbilledUsageCountsOnlyUsageNothingCovered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	free := currentCard().FreeEvents
	for name, tc := range map[string]struct {
		previous, last int64
		now            time.Time
		setup          func(t *testing.T, f *fixture)
		billingOff     bool
		want           int
	}{
		"over its allowance":           {last: free + 1, now: closeNow, want: 1},
		"at its allowance":             {last: free, now: closeNow},
		"over only in an older period": {previous: free + 1, now: closeNow},
		"over inside its grace":        {last: free + 1, now: periodEnd.Add(grace).Add(-time.Hour)},
		"with a mandate that ended as it began": {last: free + 1, now: closeNow, want: 1, setup: func(t *testing.T, f *fixture) {
			seedMandate(t, f, day(time.June, 1), periodStart)
		}},
		"with a mandate that ended inside it": {last: free + 1, now: closeNow, setup: func(t *testing.T, f *fixture) {
			seedMandate(t, f, day(time.June, 1), day(time.August, 20))
		}},
		"with a live mandate": {last: free + 1, now: closeNow, setup: func(t *testing.T, f *fixture) {
			seedMandate(t, f, day(time.June, 1), time.Time{})
		}},
		"on a deal that lapsed as it began": {last: free + 1, now: closeNow, want: 1, setup: func(t *testing.T, f *fixture) {
			setDealTerms(t, f, day(time.June, 1), periodStart)
		}},
		"on a deal that lapsed inside it": {last: free + 1, now: closeNow, setup: func(t *testing.T, f *fixture) {
			setDealTerms(t, f, day(time.June, 1), day(time.August, 20))
		}},
		"with billing off": {last: free + 1, now: closeNow, billingOff: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			seedPeriodCounts(t, f, tc.previous, tc.last)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			svc := f.svc
			if tc.billingOff {
				var err error
				if svc, err = corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, nil); err != nil {
					t.Fatalf("new service: %v", err)
				}
			}
			got, err := svc.UnbilledUsage(t.Context(), tc.now, grace)
			if err != nil {
				t.Fatalf("UnbilledUsage: %v", err)
			}
			if got != tc.want {
				t.Errorf("unbilled = %d, want %d", got, tc.want)
			}
		})
	}
}
