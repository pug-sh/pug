package usage_test

import (
	"testing"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

func utc(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// A quota window runs from the org's anniversary, so this arithmetic decides what
// every "X of Y" on the dashboard is measured over. The specific bug it exists to
// fail on: time.Date NORMALIZES an out-of-range day, turning 31 February into
// 3 March, which would put a period boundary in the wrong month entirely.
func TestPeriodForClampsShortMonths(t *testing.T) {
	cases := []struct {
		name       string
		now        time.Time
		anchorDay  int
		start, end time.Time
	}{
		{
			name:      "mid-month anchor",
			now:       time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
			anchorDay: 17,
			start:     utc(2026, 6, 17), end: utc(2026, 7, 17),
		},
		{
			// Before this month's anchor, so the period began last month.
			name:      "before the anchor falls",
			now:       time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC),
			anchorDay: 17,
			start:     utc(2026, 5, 17), end: utc(2026, 6, 17),
		},
		{
			// Half-open: an instant exactly on a boundary belongs to the LATER
			// period, or two consecutive windows would both claim it.
			name:      "exactly on the anchor",
			now:       utc(2026, 6, 17),
			anchorDay: 17,
			start:     utc(2026, 6, 17), end: utc(2026, 7, 17),
		},
		{
			name:      "anchor 31 clamps to a 28-day february",
			now:       time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC),
			anchorDay: 31,
			start:     utc(2026, 1, 31), end: utc(2026, 2, 28),
		},
		{
			// The clamp does not stick: each start is re-derived from the anchor, so
			// March goes back to the 31st rather than staying on the 28th.
			name:      "anchor 31 un-clamps the next month",
			now:       time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
			anchorDay: 31,
			start:     utc(2026, 2, 28), end: utc(2026, 3, 31),
		},
		{
			name:      "anchor 31 in a leap february",
			now:       time.Date(2028, 2, 10, 0, 0, 0, 0, time.UTC),
			anchorDay: 31,
			start:     utc(2028, 1, 31), end: utc(2028, 2, 29),
		},
		{
			name:      "anchor 30 in february",
			now:       time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
			anchorDay: 30,
			start:     utc(2026, 1, 30), end: utc(2026, 2, 28),
		},
		{
			name:      "year rollover",
			now:       time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
			anchorDay: 20,
			start:     utc(2025, 12, 20), end: utc(2026, 1, 20),
		},
		{
			// Everything is UTC end to end: 00:30 IST on the 17th is still the 16th
			// in UTC, so the anniversary has not come round yet.
			name:      "non-UTC input",
			now:       time.Date(2026, 6, 17, 0, 30, 0, 0, time.FixedZone("IST", 5*3600+1800)),
			anchorDay: 17,
			start:     utc(2026, 5, 17), end: utc(2026, 6, 17),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := coreusage.PeriodFor(tc.now, tc.anchorDay)
			if !start.Equal(tc.start) || !end.Equal(tc.end) {
				t.Errorf("PeriodFor(%s, %d) = [%s, %s), want [%s, %s)",
					tc.now, tc.anchorDay, start, end, tc.start, tc.end)
			}
		})
	}
}

// Consecutive windows must tile the calendar exactly: a gap loses a day's events
// from every total, an overlap counts one twice.
func TestPeriodsTileWithoutGapOrOverlap(t *testing.T) {
	for anchor := 1; anchor <= 31; anchor++ {
		cursor := utc(2026, 1, 1)
		_, end := coreusage.PeriodFor(cursor, anchor)

		for range 24 {
			// The instant the window ends is the instant the next one starts.
			nextStart, nextEnd := coreusage.PeriodFor(end, anchor)
			if !nextStart.Equal(end) {
				t.Fatalf("anchor %d: window after %s starts at %s, leaving a %s hole",
					anchor, end, nextStart, nextStart.Sub(end))
			}
			if !nextEnd.After(nextStart) {
				t.Fatalf("anchor %d: window [%s, %s) is empty or inverted", anchor, nextStart, nextEnd)
			}
			// And the instant just before it belongs to the window that ended.
			prevStart, prevEnd := coreusage.PeriodFor(end.Add(-time.Nanosecond), anchor)
			if !prevEnd.Equal(end) {
				t.Fatalf("anchor %d: the instant before %s resolves to [%s, %s), which overlaps",
					anchor, end, prevStart, prevEnd)
			}
			end = nextEnd
		}
	}
}

// Every period bound must land on midnight: RefreshPeriodUsage sums usage_daily
// with CeilDayUTC on both ends, which is exact only for day-aligned windows. A
// bound carrying a time of day would silently drop a partial day from the total.
func TestPeriodBoundsAreMidnightAligned(t *testing.T) {
	for anchor := 1; anchor <= 31; anchor++ {
		for month := time.January; month <= time.December; month++ {
			now := time.Date(2026, month, 14, 7, 31, 12, 500, time.UTC)
			start, end := coreusage.PeriodFor(now, anchor)
			for _, bound := range []time.Time{start, end} {
				if h, m, s := bound.Clock(); h != 0 || m != 0 || s != 0 || bound.Nanosecond() != 0 {
					t.Fatalf("anchor %d, %s: bound %s is not midnight", anchor, month, bound)
				}
			}
		}
	}
}

func TestAnchorDayFallsBackToTheOrgCreationDay(t *testing.T) {
	created := time.Date(2026, 3, 17, 14, 32, 0, 0, time.UTC)

	if got := coreusage.AnchorDay(created, 0); got != 17 {
		t.Errorf("AnchorDay with no override = %d, want the creation day 17", got)
	}
	if got := coreusage.AnchorDay(created, 5); got != 5 {
		t.Errorf("AnchorDay with an override = %d, want 5", got)
	}
	// Out of range cannot be stored (the column's check constraint) and must not
	// produce a period nobody can compute anyway.
	if got := coreusage.AnchorDay(created, 99); got != 17 {
		t.Errorf("AnchorDay with a nonsense override = %d, want the creation day 17", got)
	}
	// The creation instant is read in UTC: 01:00 IST on the 17th is the 16th.
	ist := time.Date(2026, 3, 17, 1, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	if got := coreusage.AnchorDay(ist, 0); got != 16 {
		t.Errorf("AnchorDay from a non-UTC creation = %d, want 16", got)
	}
}
