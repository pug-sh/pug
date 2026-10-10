package retention

import (
	"time"

	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// wait is how long a shorter length waits before it replaces the enforced one.
const wait = 30 * 24 * time.Hour

// state is an org's retention_state row. Zero days keeps everything, and so does
// an org with no row.
type state struct {
	days        int64
	pendingDays int64
	// Zero beside pendingDays waits for an expire.
	pendingSince time.Time
	expiredBy    string
}

func stateFromRow(row dbread.RetentionState) state {
	return state{
		days:         row.Days.Int64,
		pendingDays:  row.PendingDays.Int64,
		pendingSince: row.PendingSince.Time.UTC(),
		expiredBy:    row.ExpiredBy.String,
	}
}

func (s state) params(orgID string) dbwrite.UpsertRetentionStateParams {
	return dbwrite.UpsertRetentionStateParams{
		Days:         postgres.NewOptionalInt8(s.days),
		ExpiredBy:    postgres.NewOptionalText(s.expiredBy),
		OrgID:        orgID,
		PendingDays:  postgres.NewOptionalInt8(s.pendingDays),
		PendingSince: postgres.NewOptionalTimestamptz(s.pendingSince),
	}
}

// keeps reports whether length a keeps at least as much as b. Zero keeps
// everything.
func keeps(a, b int64) bool { return a == 0 || (b != 0 && a >= b) }

// drops reports a new shorter length, one that next makes pending.
func (s state) drops(days int64) bool { return !keeps(days, s.days) && days != s.pendingDays }

// next is the state after a run resolved the org's length to days. lapsed holds
// a new drop for an expire instead of starting its wait.
//
// It never carries an expire over into a state it changes, so a run that read
// the row before an expire landed cannot undo it: it writes only on a new
// length, which supersedes the expire anyway.
func (s state) next(days int64, lapsed bool, now time.Time) state {
	switch {
	case keeps(days, s.days):
		return state{days: days}
	case s.drops(days):
		next := state{days: s.days, pendingDays: days}
		if !lapsed {
			next.pendingSince = now
		}
		return next
	case !s.pendingSince.IsZero() && now.Sub(s.pendingSince) >= wait:
		return state{days: days}
	}
	return s
}
