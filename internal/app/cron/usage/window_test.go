package usage

import (
	"testing"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

func utc(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// A full pass must never be narrower than month-to-date. An org anchored on the
// 24th has a current period starting the 24th, which is LATER than the trailing
// rescan floor, so widening to the earliest period alone leaves the pass covering
// two days -- too narrow to reconcile an erasure, and too little evidence for the
// empty-read guard to tell an idle deployment from a bad ClickHouse read.
func TestFullPassNeverNarrowerThanMonthToDate(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	clustered := []coreusage.OrgPeriod{{OrgID: "o_1", Start: utc(2026, time.August, 24)}}

	if got := meterFrom(now, 2, true, clustered, time.Time{}); !got.Equal(utc(2026, time.August, 1)) {
		t.Errorf("full pass from = %s, want 2026-08-01 (month-to-date)", got)
	}
}

// An anniversary reaching into the previous month widens it further: stopping at
// the month boundary would re-sum that org over a window the pass had not read.
func TestFullPassWidensToAnAnniversaryBeforeTheMonth(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	spread := []coreusage.OrgPeriod{
		{OrgID: "o_1", Start: utc(2026, time.August, 24)},
		{OrgID: "o_2", Start: utc(2026, time.July, 26)},
	}

	if got := meterFrom(now, 2, true, spread, time.Time{}); !got.Equal(utc(2026, time.July, 26)) {
		t.Errorf("full pass from = %s, want 2026-07-26 (the earliest anniversary)", got)
	}
}

// A caught-up non-full pass is the trailing rescan alone, whatever the anchors say.
func TestNonFullPassIsTheTrailingRescan(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	spread := []coreusage.OrgPeriod{{OrgID: "o_1", Start: utc(2026, time.July, 26)}}

	if got := meterFrom(now, 2, false, spread, time.Time{}); !got.Equal(utc(2026, time.August, 23)) {
		t.Errorf("incremental pass from = %s, want 2026-08-23", got)
	}
}

// No orgs at all still gets the month floor rather than a zero time.
func TestFullPassWithNoOrgsFloorsAtTheMonth(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	if got := meterFrom(now, 2, true, nil, time.Time{}); !got.Equal(utc(2026, time.August, 1)) {
		t.Errorf("full pass with no orgs from = %s, want 2026-08-01", got)
	}
}

// A meter back from an outage re-reads from its last successful pass's window.
func TestPassCatchesUpFromTheLastSuccessfulPass(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		lastMetered time.Time
		full        bool
		want        time.Time
	}{
		{"never metered is the trailing rescan", time.Time{}, false, utc(2026, time.August, 23)},
		{"a recent pass does not narrow the rescan", now.Add(-time.Hour), false, utc(2026, time.August, 23)},
		{"an outage widens to the last pass's window", time.Date(2026, time.August, 18, 22, 0, 0, 0, time.UTC), false, utc(2026, time.August, 16)},
		{"an outage wider than a full pass wins", time.Date(2026, time.July, 20, 1, 0, 0, 0, time.UTC), true, utc(2026, time.July, 18)},
		{"a long outage stops at retention", utc(2024, time.January, 1), false, coreusage.FloorDayUTC(now.Add(-retention))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := meterFrom(now, 2, tc.full, nil, tc.lastMetered); !got.Equal(tc.want) {
				t.Errorf("from = %s, want %s", got, tc.want)
			}
		})
	}
}
