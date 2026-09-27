package meter

import "time"

// Window is a period's run of whole UTC days, [Start, End). usage_daily counts
// days while the provider's periods start at an instant; giving every day to
// exactly one window is what reconciles the two.
type Window struct{ Start, End time.Time }

// day is the UTC midnight that starts t's UTC day.
func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// windowFor derives the window of the period [periodStart, periodEnd). An org's
// windows are contiguous and never overlap: a period already stated keeps the start
// it was first given, and a new one starts where the org's previous window ended —
// or at its own start day, after a gap. Every UTC day is therefore stated at most
// once, across renewals, re-subscriptions and deal cutovers alike: overlap days stay
// with the old window, and gap days, when the org held no subscription, fall in none.
func windowFor(periodStart, periodEnd time.Time, stated, prev *Window) Window {
	end := day(periodEnd)
	start := day(periodStart)
	switch {
	case stated != nil:
		start = stated.Start
	case prev != nil && prev.End.After(start):
		start = prev.End
	}
	if start.After(end) {
		start = end
	}
	return Window{Start: start, End: end}
}

// through is where the window's metered days end as of now: tomorrow's midnight, so
// today's partial count is included, but never past End — the renewal day belongs to
// the next window.
func (w Window) through(now time.Time) time.Time {
	if tomorrow := day(now).AddDate(0, 0, 1); tomorrow.Before(w.End) {
		return tomorrow
	}
	return w.End
}

// contiguousWith reports whether next starts exactly where w ended: only then does w
// carry into it.
func (w Window) contiguousWith(next Window) bool { return w.End.Equal(next.Start) }
