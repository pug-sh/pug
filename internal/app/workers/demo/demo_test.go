package demo

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
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

// TestDecideProfileHeal pins the restart-reconciliation boundary: an empty
// Postgres side has nothing to copy from (warn), a ClickHouse count behind
// Postgres means a copy didn't finish (re-copy), and ClickHouse in sync or
// ahead (live signups are ClickHouse-only) is healthy (skip).
func TestDecideProfileHeal(t *testing.T) {
	tests := []struct {
		name string
		ch   uint64
		pg   int64
		want profileHealAction
	}{
		{"postgres empty", 0, 0, profileHealNoSource},
		{"postgres empty but clickhouse has live rows", 5, 0, profileHealNoSource},
		{"clickhouse behind (partial copy)", 1000, 6000, profileHealRecopy},
		{"clickhouse empty, postgres seeded", 0, 6000, profileHealRecopy},
		{"in sync", 6000, 6000, profileHealOK},
		{"clickhouse ahead (live signups)", 6100, 6000, profileHealOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideProfileHeal(tt.ch, tt.pg); got != tt.want {
				t.Errorf("decideProfileHeal(%d, %d) = %d, want %d", tt.ch, tt.pg, got, tt.want)
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

// newTestWorker builds a worker with the ensured-set initialized and a
// substitute profile inserter — the minimum ensureProfile needs without a
// ClickHouse connection.
func newTestWorker(insert func(context.Context, driver.Conn, string, seed.LiveProfile) error) *worker {
	return &worker{
		ensured:       make(map[string]struct{}),
		insertProfile: insert,
	}
}

// TestEnsureProfileDedupAndSkipsBots pins that a profile is created once per
// human user per run and never for a bot id (so bots stay profile-less).
func TestEnsureProfileDedupAndSkipsBots(t *testing.T) {
	var calls atomic.Int64
	w := newTestWorker(func(_ context.Context, _ driver.Conn, _ string, _ seed.LiveProfile) error {
		calls.Add(1)
		return nil
	})
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

// TestEnsureProfilePayload pins the LiveProfile the worker hands to the insert:
// the id is the session's distinct id, create_time is the user's join, and the
// properties + external id are exactly DemoProfileProperties(idx) — the
// determinism the ReplacingMergeTree re-create relies on. A swapped or wrong-index
// field here would silently desync the live-created profile from its backfilled
// twin (flipping properties or create_time on the merge instead of being a no-op).
func TestEnsureProfilePayload(t *testing.T) {
	const distinctID = "user-00007"
	idx, ok := seed.HumanUserIndex(distinctID)
	if !ok {
		t.Fatalf("HumanUserIndex(%q) not ok", distinctID)
	}
	wantProps, wantExternalID := pgseed.DemoProfileProperties(idx)
	wantJoin := seed.DemoUserAt(idx).Join

	var got seed.LiveProfile
	w := newTestWorker(func(_ context.Context, _ driver.Conn, _ string, p seed.LiveProfile) error {
		got = p
		return nil
	})
	w.ensureProfile(context.Background(), distinctID)

	if got.ID != distinctID {
		t.Errorf("ID = %q, want %q", got.ID, distinctID)
	}
	if got.ExternalID != wantExternalID {
		t.Errorf("ExternalID = %q, want %q", got.ExternalID, wantExternalID)
	}
	if !got.CreateTime.Equal(wantJoin) {
		t.Errorf("CreateTime = %v, want %v (user join)", got.CreateTime, wantJoin)
	}
	if !reflect.DeepEqual(got.Properties, wantProps) {
		t.Errorf("Properties = %v, want %v", got.Properties, wantProps)
	}
}

// TestEnsureProfileRetriesOnFailure pins the best-effort retry: a failed insert
// unmarks the user so the next session re-attempts, and a later success marks it
// so it isn't created a third time.
func TestEnsureProfileRetriesOnFailure(t *testing.T) {
	var calls atomic.Int64
	w := newTestWorker(func(_ context.Context, _ driver.Conn, _ string, _ seed.LiveProfile) error {
		if calls.Add(1) == 1 {
			return errors.New("boom") // first attempt fails
		}
		return nil
	})
	ctx := context.Background()

	w.ensureProfile(ctx, "user-00002") // fails → unmark
	w.ensureProfile(ctx, "user-00002") // retries → succeeds, marks
	w.ensureProfile(ctx, "user-00002") // already marked → no further call
	if got := calls.Load(); got != 2 {
		t.Fatalf("insert called %d times, want 2 (one failure + one success)", got)
	}
}
