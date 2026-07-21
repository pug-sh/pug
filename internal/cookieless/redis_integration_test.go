package cookieless

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

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

// TestSessionID_OutOfOrderDoesNotFragment pins the watermark's monotonicity.
//
// An event landing more than sessionInactivity BEFORE the recorded last activity
// belongs to a genuinely different (earlier) session — but recording ITS time as
// the new last-activity rewrites the watermark backwards. The next event at the
// original time then measures its gap against the rewound watermark, sees a fresh
// gap, and mints again: two events with the SAME occur_time land in different
// sessions, which no session model permits.
//
// Cross-batch by construction — each call is its own request. Sorting a batch by
// occur_time cannot fix this, since batch 2 cannot be sorted against batch 1's
// already-committed watermark. The guard has to live on the write.
func TestSessionID_OutOfOrderDoesNotFragment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return base }
	const did = IDPrefix + "ooo"

	// Far enough back that withinInactivity is false in both directions.
	lateGap := sessionInactivity + 20*time.Minute
	late := base.Add(-lateGap)

	first, _ := r.SessionID(ctx, "p1", did, "20260720", base)
	stale, _ := r.SessionID(ctx, "p1", did, "20260720", late)
	again, _ := r.SessionID(ctx, "p1", did, "20260720", base)

	if stale == first {
		t.Errorf("event %v before last activity is past the inactivity window and must not join that session", lateGap)
	}
	if again != first {
		t.Errorf("identical occur_time must resolve to one session, got %q then %q: the late event rewrote the watermark backwards", first, again)
	}
}

// raceHook installs a value for key the first time a GET on it misses, so the
// SetNX saltForDay issues immediately afterwards loses the mint race. That
// window is microseconds wide in production, which is exactly why it has to be
// driven deterministically rather than by racing goroutines and hoping.
type raceHook struct {
	rdb    *redis.Client
	key    string
	winner string
	once   sync.Once
}

func (h *raceHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *raceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *raceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() != "get" || !errors.Is(err, redis.Nil) {
			return err
		}
		if len(cmd.Args()) < 2 || fmt.Sprint(cmd.Args()[1]) != h.key {
			return err
		}
		// Stand in for another pod winning the mint. The SET re-enters this hook
		// with name "set", which returns above, so there is no recursion.
		h.once.Do(func() { h.rdb.Set(ctx, h.key, h.winner, time.Hour) })
		return err
	}
}

// TestSaltForDay_LostMintRace_AdoptsWinner covers the SETNX-loser branch: when
// two pods both observe an absent salt, the loser must adopt the winner's value
// rather than keep the one it minted locally. A fork here means the same visitor
// hashes to two different cookieless ids on the same day — one person counted as
// two, on every pod that lost, for a whole day.
func TestSaltForDay_LostMintRace_AdoptsWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	const day = "20260720"

	winner := make([]byte, saltLen)
	for i := range winner {
		winner[i] = byte(i + 1)
	}
	rd.Client.AddHook(&raceHook{
		rdb:    rd.Client,
		key:    saltKeyPrefix + day,
		winner: base64.StdEncoding.EncodeToString(winner),
	})

	r := New(rd.Client)
	got, err := r.saltForDay(ctx, day)
	if err != nil {
		t.Fatalf("losing the mint race must adopt the winner, not error: %v", err)
	}
	if !bytes.Equal(got, winner) {
		t.Fatalf("adopted salt = %x, want the winner's %x — the fleet forked", got, winner)
	}

	// The adopted value must also be what lands in the cache, or the next call
	// on this pod re-forks against the winner it just agreed with.
	cached, err := r.saltForDay(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, winner) {
		t.Errorf("cached salt = %x, want %x", cached, winner)
	}
}
