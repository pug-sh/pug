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

	if got := meterFrom(now, 2, true, clustered); !got.Equal(utc(2026, time.August, 1)) {
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

	if got := meterFrom(now, 2, true, spread); !got.Equal(utc(2026, time.July, 26)) {
		t.Errorf("full pass from = %s, want 2026-07-26 (the earliest anniversary)", got)
	}
}

// A non-full pass is the trailing rescan alone, whatever the anchors say.
func TestNonFullPassIsTheTrailingRescan(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	spread := []coreusage.OrgPeriod{{OrgID: "o_1", Start: utc(2026, time.July, 26)}}

	if got := meterFrom(now, 2, false, spread); !got.Equal(utc(2026, time.August, 23)) {
		t.Errorf("incremental pass from = %s, want 2026-08-23", got)
	}
}

// No orgs at all still gets the month floor rather than a zero time.
func TestFullPassWithNoOrgsFloorsAtTheMonth(t *testing.T) {
	now := time.Date(2026, time.August, 25, 6, 0, 0, 0, time.UTC)
	if got := meterFrom(now, 2, true, nil); !got.Equal(utc(2026, time.August, 1)) {
		t.Errorf("full pass with no orgs from = %s, want 2026-08-01", got)
	}
}
