package billing

import (
	"testing"
	"time"
)

// Every row of §8.7's table.
func TestDeferCloseFollowsTheTable(t *testing.T) {
	for _, tc := range []struct {
		name           string
		usage, balance int64
		sweep          bool
		want           deferral
	}{
		{"nothing to bill waits the balance out", 0, 300, false, deferral{status: InvoiceWaived}},
		{"under the threshold defers", 150, 300, false, deferral{status: InvoiceDeferred}},
		{"at the threshold charges and carries", 200, 300, false, deferral{status: InvoiceOpen, carry: true}},
		{"a large close carries a balance", 5_000, 40, false, deferral{status: InvoiceOpen, carry: true}},
		{"a sweep at a dollar charges whatever the total", 0, 100, true, deferral{status: InvoiceOpen, carry: true}},
		{"a sweep under a dollar writes it all off", 20, 79, true, deferral{status: InvoiceWaived, waiveBalance: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deferClose(tc.usage, tc.balance, tc.sweep); got != tc.want {
				t.Errorf("deferClose(%d, %d, %v) = %+v, want %+v", tc.usage, tc.balance, tc.sweep, got, tc.want)
			}
		})
	}
}

func TestPeriodsSpannedCountsBothEnds(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		first, last time.Time
		want        int
	}{
		{d(2026, 1, 10), d(2026, 1, 10), 1},
		{d(2026, 1, 31), d(2026, 2, 28), 2},
		{d(2025, 9, 10), d(2026, 8, 10), 12},
	} {
		if got := periodsSpanned(tc.first, tc.last); got != tc.want {
			t.Errorf("periodsSpanned(%s, %s) = %d, want %d", tc.first, tc.last, got, tc.want)
		}
	}
}
