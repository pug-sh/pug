package cookieless

import (
	"context"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/testutil"
)

func TestSaltForDay_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	r1 := New(rd.Client)
	r1.now = func() time.Time { return now }
	r2 := New(rd.Client)
	r2.now = func() time.Time { return now }

	t.Run("two_resolvers_share_one_salt", func(t *testing.T) {
		a, err := r1.DistinctID(ctx, "20260720", "p", "ip", "ua")
		if err != nil {
			t.Fatal(err)
		}
		b, err := r2.DistinctID(ctx, "20260720", "p", "ip", "ua")
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Errorf("resolvers sharing Redis must agree: %q vs %q", a, b)
		}
	})

	t.Run("salt_key_has_ttl", func(t *testing.T) {
		ttl, err := rd.Client.TTL(ctx, saltKeyPrefix+"20260720").Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl <= 0 || ttl > saltTTL {
			t.Errorf("salt TTL = %v, want (0, %v]", ttl, saltTTL)
		}
	})

	t.Run("cache_prunes_expired_days", func(t *testing.T) {
		if _, err := r1.DistinctID(ctx, "20260720", "p", "ip", "ua"); err != nil {
			t.Fatal(err)
		}
		// Jump three days: 20260720 is now outside {today, yesterday}.
		r1.now = func() time.Time { return now.AddDate(0, 0, 3) }
		if _, err := r1.DistinctID(ctx, "20260723", "p", "ip", "ua"); err != nil {
			t.Fatal(err)
		}
		r1.mu.Lock()
		_, stale := r1.salts["20260720"]
		r1.mu.Unlock()
		if stale {
			t.Error("old salt must be pruned from process memory (re-linking hazard)")
		}
	})

	t.Run("redis_down_cold_cache_errors", func(t *testing.T) {
		cold := New(rd.Client)
		cold.now = func() time.Time { return now }
		cctx, cancel := context.WithCancel(ctx)
		cancel() // force every Redis op to fail
		if _, err := cold.DistinctID(cctx, "20260720", "p", "ip", "ua"); err == nil {
			t.Error("cold cache + unreachable Redis must error, not fabricate identity")
		}
	})

	t.Run("redis_down_warm_cache_still_hashes", func(t *testing.T) {
		warm := New(rd.Client)
		warm.now = func() time.Time { return now }
		if _, err := warm.DistinctID(ctx, "20260720", "p", "ip", "ua"); err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := warm.DistinctID(cctx, "20260720", "p", "ip", "ua"); err != nil {
			t.Errorf("warm salt cache must serve despite Redis outage: %v", err)
		}
	})
}

func TestSessionID_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return base }
	const did = IDPrefix + "abc"

	s1, degraded := r.SessionID(ctx, "p1", did, "20260720", base)
	if degraded {
		t.Fatal("healthy Redis must not degrade")
	}
	if s1 == "" {
		t.Fatal("empty session id")
	}

	t.Run("reuse_within_inactivity", func(t *testing.T) {
		s2, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(10*time.Minute))
		if s2 != s1 {
			t.Errorf("10min gap must reuse session: %q vs %q", s2, s1)
		}
	})

	t.Run("slide_extends_window", func(t *testing.T) {
		// 10min + 25min = 35min from start but only 25min from last activity.
		s3, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(35*time.Minute))
		if s3 != s1 {
			t.Errorf("sliding window must extend: %q vs %q", s3, s1)
		}
	})

	t.Run("gap_over_inactivity_mints_new", func(t *testing.T) {
		s4, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(35*time.Minute).Add(sessionInactivity+time.Minute))
		if s4 == s1 {
			t.Error("gap past inactivity must mint a new session")
		}
	})

	t.Run("distinct_visitors_do_not_share", func(t *testing.T) {
		other, _ := r.SessionID(ctx, "p1", IDPrefix+"other", "20260720", base)
		if other == s1 {
			t.Error("different distinct_id must get its own session")
		}
	})

	t.Run("degraded_fallback_is_deterministic", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		f1, deg1 := r.SessionID(cctx, "p1", did, "20260720", base)
		f2, deg2 := r.SessionID(cctx, "p1", did, "20260720", base.Add(2*time.Hour))
		if !deg1 || !deg2 {
			t.Fatal("unreachable Redis must report degraded")
		}
		if f1 != f2 {
			t.Errorf("fallback must be one deterministic session per visitor-day: %q vs %q", f1, f2)
		}
		if f1 == s1 {
			t.Error("fallback must not collide with a minted session")
		}
	})
}
