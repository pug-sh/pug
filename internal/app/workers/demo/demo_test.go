package demo

import "testing"

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
