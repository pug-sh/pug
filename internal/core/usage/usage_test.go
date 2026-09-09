package usage_test

import (
	"testing"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
)

// These bound every period and the prune cutoff, so an off-by-one here either
// drops the 1st of a month from every total or deletes a day of retained data.
func TestDayAndMonthBoundaries(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)

	cases := []struct {
		name                 string
		in                   time.Time
		floor, ceil          time.Time
		monthStart, monthEnd time.Time
	}{
		{
			name:       "mid-day",
			in:         time.Date(2026, 6, 15, 13, 45, 0, 0, time.UTC),
			floor:      time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			ceil:       time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			monthStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			monthEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// CeilDayUTC leaves an exact midnight alone; rounding it up would shift
			// every calendar-month period a day forward.
			name:       "exact midnight",
			in:         time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			floor:      time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			ceil:       time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			monthStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			monthEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "year rollover",
			in:         time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			floor:      time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
			ceil:       time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			monthStart: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			monthEnd:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "leap february",
			in:         time.Date(2028, 2, 29, 6, 0, 0, 0, time.UTC),
			floor:      time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			ceil:       time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
			monthStart: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
			monthEnd:   time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// A non-UTC input is converted first: 00:30 IST on the 16th is still the
			// 15th in UTC, and usage is UTC end to end.
			name:       "non-UTC location",
			in:         time.Date(2026, 6, 16, 0, 30, 0, 0, kolkata),
			floor:      time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			ceil:       time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			monthStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			monthEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coreusage.FloorDayUTC(tc.in); !got.Equal(tc.floor) {
				t.Errorf("FloorDayUTC = %s, want %s", got, tc.floor)
			}
			if got := coreusage.CeilDayUTC(tc.in); !got.Equal(tc.ceil) {
				t.Errorf("CeilDayUTC = %s, want %s", got, tc.ceil)
			}
			start, end := coreusage.PeriodFor(tc.in, 1)
			if !start.Equal(tc.monthStart) || !end.Equal(tc.monthEnd) {
				t.Errorf("PeriodFor = [%s, %s), want [%s, %s)", start, end, tc.monthStart, tc.monthEnd)
			}
		})
	}
}
