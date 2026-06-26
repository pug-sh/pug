package demo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
)

// TestDecideSeedAction pins the backfill gate's boundary: a completed backfill
// (n >= SeedCount) skips, an interrupted one (0 < n < SeedCount) warns rather
// than re-running into duplicates, and an empty project backfills.
func TestDecideSeedAction(t *testing.T) {
	const seedCount = 500_000
	tests := []struct {
		name string
		n    uint64
		want seedAction
	}{
		{"empty project", 0, seedBackfill},
		{"one event (interrupted)", 1, seedWarnPartial},
		{"just below target", seedCount - 1, seedWarnPartial},
		{"exactly target", seedCount, seedSkip},
		{"above target (live traffic grew it)", seedCount + 1000, seedSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideSeedAction(tt.n, seedCount); got != tt.want {
				t.Errorf("decideSeedAction(%d, %d) = %d, want %d", tt.n, seedCount, got, tt.want)
			}
		})
	}
}

// TestEnabled pins the boolean parsing of PUG_DEMO_ENABLED, which gates whether
// `pug dev` auto-starts the demo worker. Note that only Go bool literals enable
// it: a human-friendly "yes" is intentionally treated as disabled.
func TestEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"t", true},
		{"TRUE", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"yes", false},
		{"on", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("PUG_DEMO_ENABLED", tt.val)
			if got := Enabled(); got != tt.want {
				t.Errorf("Enabled() with PUG_DEMO_ENABLED=%q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

// TestEnsureProfileDedupAndSkipsBots pins that a profile is created once per
// human user per run and never for a bot id (so bots stay profile-less).
func TestEnsureProfileDedupAndSkipsBots(t *testing.T) {
	var calls atomic.Int64
	w := &worker{
		insertProfile: func(_ context.Context, _ driver.Conn, _ string, _ seed.LiveProfile) error {
			calls.Add(1)
			return nil
		},
	}
	ctx := context.Background()

	w.ensureProfile(ctx, "user-00001")
	w.ensureProfile(ctx, "user-00001") // same user this run: deduped
	if got := calls.Load(); got != 1 {
		t.Fatalf("insert called %d times for one user, want 1", got)
	}

	w.ensureProfile(ctx, "bot-0001") // bots never get a profile
	if got := calls.Load(); got != 1 {
		t.Fatalf("insert called for a bot id (calls=%d)", got)
	}
}

// TestEnsureProfileRetriesOnFailure pins the best-effort retry: a failed insert
// unmarks the user so the next session re-attempts, and a later success marks it
// so it isn't created a third time.
func TestEnsureProfileRetriesOnFailure(t *testing.T) {
	var calls atomic.Int64
	w := &worker{
		insertProfile: func(_ context.Context, _ driver.Conn, _ string, _ seed.LiveProfile) error {
			if calls.Add(1) == 1 {
				return errors.New("boom") // first attempt fails
			}
			return nil
		},
	}
	ctx := context.Background()

	w.ensureProfile(ctx, "user-00002") // fails → unmark
	w.ensureProfile(ctx, "user-00002") // retries → succeeds, marks
	w.ensureProfile(ctx, "user-00002") // already marked → no further call
	if got := calls.Load(); got != 2 {
		t.Fatalf("insert called %d times, want 2 (one failure + one success)", got)
	}
}
