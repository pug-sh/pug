package retention

import "time"

// cut is where an org's deletes stop: the first day of the month that holds
// now − days, and never later than the first day of the previous month, which
// usage and billing still re-read. False cuts nothing.
func cut(now time.Time, days int64) (time.Time, bool) {
	now = now.UTC()
	// Checked before AddDate, which wraps a length near MaxInt64 to a date in the
	// future. A cut before 1970 binds as no DateTime, and a length that long keeps
	// everything anyway.
	if days <= 0 || days > int64(now.Sub(time.Unix(0, 0))/(24*time.Hour)) {
		return time.Time{}, false
	}
	at := firstOfMonth(now.AddDate(0, 0, -int(days)))
	if latest := firstOfMonth(now).AddDate(0, -1, 0); latest.Before(at) {
		return latest, true
	}
	return at, true
}

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
