// Package cron holds what pug's scheduled jobs share: their advisory-lock keys
// and their persisted task state.
package cron

// LockKey is a job's Postgres advisory-lock id, held for the length of one pass.
// iota rather than hand-picked values because a collision is silent and reads as
// success: the second job finds the lock held and exits 0 having done nothing.
type LockKey int64

const (
	LockUsage LockKey = iota + 1

	// Keep last: the count the test checks allLockKeys against.
	lockKeyCount = iota
)

var allLockKeys = []LockKey{LockUsage}
