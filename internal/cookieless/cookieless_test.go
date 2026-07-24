package cookieless

import (
	"context"
	"encoding/base64"
	"errors"
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
		day   Day
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

// TestStoreSalt_RejectsCorrupt pins the rejection that stands between a corrupt
// Redis value and a fabricated identity. hmac.New accepts ANY key length, so
// without this check a truncated or empty salt still mints confident-looking
// ids — failing open on the one primitive the privacy guarantee rests on.
// Mutation-verified: deleting the length/decode check leaves the suite green.
func TestStoreSalt_RejectsCorrupt(t *testing.T) {
	r := fixedResolver(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	long := base64.StdEncoding.EncodeToString(make([]byte, saltLen+1))

	for _, c := range []struct{ name, val string }{
		{"not_base64", "!!!not-base64!!!"},
		{"empty", ""},
		{"too_short", short},
		{"too_long", long},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.storeSalt("20260720", c.val)
			if err == nil {
				t.Fatalf("storeSalt(%q) = (%x, nil), want an error — a corrupt salt must never key an HMAC", c.val, got)
			}
			if got != nil {
				t.Errorf("rejected salt must return nil bytes, got %x", got)
			}
			r.mu.Lock()
			_, cached := r.salts["20260720"]
			r.mu.Unlock()
			if cached {
				t.Error("a rejected salt must not be cached")
			}
		})
	}
}

// TestSaltForDay_RejectsDayOutsideWindow moves the accepted-window invariant from
// caller discipline into the boundary that depends on it. DayOf computes the
// window and the ingest handler honours it, but saltForDay itself accepted any
// string — so a malformed or out-of-window day minted a real salt and persisted
// it under its own key, outliving every code path that could use it.
func TestSaltForDay_RejectsDayOutsideWindow(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	// nil Redis client: a rejected day must fail before any Redis call, so a
	// panic here is itself the failure signal.
	r := fixedResolver(now)

	for _, c := range []struct {
		name string
		day  Day
	}{
		{"empty", ""},
		{"malformed", "not-a-day"},
		{"two_days_ago", "20260718"},
		{"tomorrow", "20260721"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := r.saltForDay(context.Background(), c.day); err == nil {
				t.Errorf("saltForDay(%q) = nil error, want rejection — an out-of-window day must never mint a salt", c.day)
			}
		})
	}
}

// TestSaltTTLFor_DayAligned pins the salt's deletion instant to the moment it
// leaves DayOf's accepted window, rather than 72h after whenever it happened to
// be minted.
//
// The TTL is the privacy guarantee, so its anchor matters: SetNX stamps expiry at
// mint, and a salt is minted lazily on the first event attributed to that day —
// which an offline-buffered flush can push to nearly D+2. A flat 72h therefore
// kept a salt re-derivable until as late as D+5, and made "at most two salts are
// ever live" false (up to five coexist). Anchoring to D+2 00:00 UTC makes that
// claim true by construction.
func TestSaltTTLFor_DayAligned(t *testing.T) {
	for _, c := range []struct {
		name string
		now  time.Time
		day  Day
		want time.Duration
	}{
		{"minted_at_day_start", time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), "20260720", 48 * time.Hour},
		{"minted_midday", time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC), "20260720", 39 * time.Hour},
		{"minted_as_yesterday_late", time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC), "20260720", time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := fixedResolver(c.now)
			got, err := r.saltTTLFor(c.day)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("saltTTLFor(%s) at %v = %v, want %v (expiry must be D+2 00:00 UTC)", c.day, c.now, got, c.want)
			}
			if got > 48*time.Hour {
				t.Errorf("TTL %v exceeds the 48h accepted window — the salt would outlive every path that can use it", got)
			}
		})
	}
}

// TestStaleSessionID_PerWindow pins the resolution of the session-semantics TODO:
// stranded events (arriving more than sessionInactivity BEFORE the live
// watermark) group by their own inactivity-sized window, deterministically.
//
// Deterministic matters more than the grouping: BatchCreate is client-retryable,
// and a random id per call would write a fresh session row into
// dashboard_session_rollup (keyed by session_id) on every retry.
func TestStaleSessionID_PerWindow(t *testing.T) {
	r := fixedResolver(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	const did = IDPrefix + "abc"
	const day Day = "20260720"
	base := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)

	a := r.staleSessionID(did, day, base)
	if a == "" {
		t.Fatal("stale session id must not be empty")
	}
	if again := r.staleSessionID(did, day, base); again != a {
		t.Errorf("same inputs must be idempotent (client retry): %q vs %q", a, again)
	}
	if near := r.staleSessionID(did, day, base.Add(time.Minute)); near != a {
		t.Errorf("events 1min apart share a window and must share a session: %q vs %q", near, a)
	}
	if far := r.staleSessionID(did, day, base.Add(sessionInactivity+time.Minute)); far == a {
		t.Error("events more than sessionInactivity apart must land in different windows")
	}
	if other := r.staleSessionID(IDPrefix+"other", day, base); other == a {
		t.Error("different visitor must not share a stranded session")
	}
	// Must stay distinguishable from the Redis-outage fallback, which is the
	// hazard the TODO named for the per-day candidate.
	if a == r.fallbackSessionID(did, day) {
		t.Error("stranded-window session must not collide with the degraded day fallback")
	}
}

// TestStoreSalt_CorruptIsDistinguishable pins that a corrupt salt is reportable
// as its own condition.
//
// It shares the salt_unavailable drop path with a Redis outage, but the two need
// opposite operator responses: an outage clears itself and the same payload
// succeeds on retry, whereas a corrupt value is re-read and re-rejected until
// the key expires — the drop reason's own comment ("the same payload may succeed
// on retry") is false for it. Only this code writes the key, so a corrupt value
// implies an external writer or a saltLen change mid-rolling-deploy; either way
// it needs a human, not a retry.
func TestStoreSalt_CorruptIsDistinguishable(t *testing.T) {
	r := fixedResolver(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))

	_, err := r.storeSalt("20260720", "!!!not-base64!!!")
	if err == nil {
		t.Fatal("corrupt salt must error")
	}
	if !errors.Is(err, ErrCorruptSalt) {
		t.Errorf("corrupt salt error = %v, want it to wrap ErrCorruptSalt so the caller can report it apart from a transient outage", err)
	}
	// The underlying cause must survive: "bad base64" and "right base64, wrong
	// length" are different operational stories and the message has to say which.
	if !strings.Contains(err.Error(), "20260720") {
		t.Errorf("error %q should name the day whose salt is corrupt", err)
	}
}

// TestDayOf_MidnightEdges pins the exact instants the accepted window opens and
// closes. The window is the salt-lifecycle boundary, so an off-by-one here
// either drops a legitimate offline flush or admits a day whose salt has gone.
func TestDayOf_MidnightEdges(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	r := fixedResolver(now)
	yesterdayMidnight := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)

	if day, ok := r.DayOf(yesterdayMidnight); !ok || day != "20260719" {
		t.Errorf("DayOf(yesterday 00:00:00.000) = (%q,%v), want (\"20260719\",true) — the window's first instant", day, ok)
	}
	if day, ok := r.DayOf(yesterdayMidnight.Add(-time.Nanosecond)); ok {
		t.Errorf("DayOf(yesterday 00:00 minus 1ns) = (%q,true), want rejected — one nanosecond outside the window", day)
	}
	todayMidnight := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if day, ok := r.DayOf(todayMidnight); !ok || day != "20260720" {
		t.Errorf("DayOf(today 00:00) = (%q,%v), want (\"20260720\",true)", day, ok)
	}
}
