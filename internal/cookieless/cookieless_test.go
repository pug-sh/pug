package cookieless

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fixedResolver(now time.Time) *Resolver {
	r := New(nil)
	r.now = func() time.Time { return now }
	return r
}

func TestDayOf(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	r := fixedResolver(now)
	cases := []struct {
		name  string
		occur time.Time
		day   string
		ok    bool
	}{
		{"today", now.Add(-1 * time.Hour), "20260720", true},
		{"yesterday", now.Add(-20 * time.Hour), "20260719", true},
		{"two_days_ago", now.Add(-40 * time.Hour), "20260718", false},
		{"tomorrow_clock_skew", now.Add(15 * time.Hour), "20260721", false},
		{"non_utc_input_normalized", time.Date(2026, 7, 20, 1, 0, 0, 0,
			time.FixedZone("IST", 5*3600+1800)), "20260719", true}, // 01:00 IST = 19:30 UTC prev day
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			day, ok := r.DayOf(tc.occur)
			if day != tc.day || ok != tc.ok {
				t.Errorf("DayOf(%v) = (%q,%v), want (%q,%v)", tc.occur, day, ok, tc.day, tc.ok)
			}
		})
	}
}

func TestDistinctID_DeterministicAndRotating(t *testing.T) {
	ctx := context.Background()
	r := fixedResolver(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	// Seed salts directly — salt fetch (Redis) is covered by integration tests.
	r.salts["20260720"] = []byte("0123456789abcdef0123456789abcdef")
	r.salts["20260719"] = []byte("fedcba9876543210fedcba9876543210")

	a1, err := r.DistinctID(ctx, "20260720", "proj1", "203.0.113.7", "Mozilla/5.0")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := r.DistinctID(ctx, "20260720", "proj1", "203.0.113.7", "Mozilla/5.0")
	if a1 != a2 {
		t.Errorf("same inputs must hash identically: %q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, IDPrefix) {
		t.Errorf("id %q must carry prefix %q", a1, IDPrefix)
	}
	if len(a1) != len(IDPrefix)+22 {
		t.Errorf("id %q length = %d, want prefix+22", a1, len(a1))
	}

	rotated, _ := r.DistinctID(ctx, "20260719", "proj1", "203.0.113.7", "Mozilla/5.0")
	if rotated == a1 {
		t.Error("different day (different salt) must produce a different id")
	}
	otherProj, _ := r.DistinctID(ctx, "20260720", "proj2", "203.0.113.7", "Mozilla/5.0")
	if otherProj == a1 {
		t.Error("different project must produce a different id (no cross-project linkage)")
	}
	otherUA, _ := r.DistinctID(ctx, "20260720", "proj1", "203.0.113.7", "Mozilla/6.0")
	if otherUA == a1 {
		t.Error("different UA must produce a different id")
	}
}

// TestParseSession_Corrupt covers both of parseSession's failure returns. A
// corrupt value is not hypothetical: the session key is a bare string another
// process could overwrite, and both failures must degrade to "mint a new
// session" rather than panicking or returning a zero time that withinInactivity
// would then read as a 56-year gap.
func TestParseSession_Corrupt(t *testing.T) {
	const sid = "f47ac10b-58cc-4372-a567-0e02b2c3d999"
	for _, c := range []struct{ name, val string }{
		{"empty", ""},
		{"no_separator", sid},
		{"non_numeric_timestamp", sid + "|not-a-number"},
		{"empty_session_id", "|1700000000000"},
		{"separator_only", "|"},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotSID, gotLast, ok := parseSession(c.val)
			if ok {
				t.Fatalf("parseSession(%q) = (%q, %v, true), want ok=false", c.val, gotSID, gotLast)
			}
			if gotSID != "" || !gotLast.IsZero() {
				t.Errorf("failed parse must return zero values, got (%q, %v)", gotSID, gotLast)
			}
		})
	}
}

func TestParseSession_RoundTrip(t *testing.T) {
	const sid = "f47ac10b-58cc-4372-a567-0e02b2c3d999"
	last := time.UnixMilli(1700000000123).UTC()

	gotSID, gotLast, ok := parseSession(formatSession(sid, last))
	if !ok {
		t.Fatal("round-trip must parse")
	}
	if gotSID != sid || !gotLast.Equal(last) {
		t.Errorf("round-trip = (%q, %v), want (%q, %v)", gotSID, gotLast, sid, last)
	}
}
