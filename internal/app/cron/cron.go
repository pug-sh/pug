// Package cron holds what pug's scheduled jobs share: their advisory-lock keys
// and their persisted task state.
package cron

// LockKey is a job's Postgres advisory-lock id, held for the length of one pass.
// iota rather than hand-picked values because a collision is silent and reads as
// success: the second job finds the lock held and exits 0 having done nothing.
//
// iota only rules that out among pug's own jobs. pg_try_advisory_xact_lock takes
// one global 64-bit keyspace per database, shared with anything else that takes an
// advisory lock there, so the keys start at a distinctive offset rather than at 1
// -- the value an unrelated tool is likeliest to pick.
type LockKey int64

// "pug" high bytes -- arbitrary, just far from any hand-picked small integer.
const lockKeyNamespace = 0x7075670000000000

const (
	LockUsage LockKey = lockKeyNamespace + iota + 1

	// Keep last: the count the test checks allLockKeys against.
	lockKeyCount = iota
)

var allLockKeys = []LockKey{LockUsage}
