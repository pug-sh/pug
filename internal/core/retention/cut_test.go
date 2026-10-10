package retention

import (
	"math"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestCut(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)
	for _, tc := range []struct {
		name string
		days int64
		now  time.Time
		want time.Time
	}{
		// The doc's table.
		{"a week keeps two months", 7, day(2026, 10, 9), day(2026, 9, 1)},
		{"90 days", 90, day(2026, 10, 9), day(2026, 7, 1)},
		{"a year", 365, day(2027, 10, 9), day(2026, 10, 1)},
		{"five years over a leap day", 1825, day(2031, 7, 15), day(2026, 7, 1)},

		{"January's previous month is December", 1, day(2027, 1, 15), day(2026, 12, 1)},
		{"the last instant of a month", 30, time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC), day(2026, 9, 1)},
		{"months are UTC's", 1, time.Date(2026, 10, 1, 2, 0, 0, 0, kolkata), day(2026, 8, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cut(tc.now, tc.days)
			if !ok || !got.Equal(tc.want) {
				t.Errorf("cut(%s, %d) = %s, %t; want %s", tc.now, tc.days, got, ok, tc.want)
			}
		})
	}
}

func TestCutKeepsEverything(t *testing.T) {
	now := day(2026, 10, 9)
	sinceEpoch := int64(now.Sub(time.Unix(0, 0)) / (24 * time.Hour))
	if got, ok := cut(now, sinceEpoch); !ok || !got.Equal(day(1970, 1, 1)) {
		t.Errorf("cut back to 1970 = %s, %t; want 1970-01-01", got, ok)
	}
	// MaxInt64 is the one AddDate wraps to tomorrow, which would cut all but a month.
	for _, days := range []int64{0, -1, sinceEpoch + 1, 1 << 40, math.MaxInt64} {
		if got, ok := cut(now, days); ok {
			t.Errorf("cut(%d) = %s, want none", days, got)
		}
	}
}
