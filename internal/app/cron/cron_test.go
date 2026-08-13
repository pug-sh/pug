package cron

import "testing"

// iota already guarantees this. The test is here for the day someone assigns an
// explicit value, because a duplicate key fails silently: the second job sees the
// lock held, decides another pass is covering it, and exits 0 having done nothing.
func TestLockKeysAreUnique(t *testing.T) {
	seen := map[LockKey]bool{}
	for _, k := range allLockKeys {
		if seen[k] {
			t.Errorf("lock key %d is declared twice", k)
		}
		seen[k] = true
	}
	// Catches a key declared without being added here, which would otherwise leave
	// the uniqueness check above asserting over a stale list.
	if len(allLockKeys) != int(lockKeyCount) {
		t.Errorf("allLockKeys has %d entries, want %d declared keys", len(allLockKeys), lockKeyCount)
	}
}
