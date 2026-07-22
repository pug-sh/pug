package cookieless

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	t.Run("salt_key_ttl_expires_when_the_day_leaves_the_window", func(t *testing.T) {
		ttl, err := rd.Client.TTL(ctx, saltKeyPrefix+"20260720").Result()
		if err != nil {
			t.Fatal(err)
		}
		// Minted at 10:00 on day D, so it must die at D+2 00:00 — 38h later,
		// not saltMaxTTL after the mint. Anchored to the day, not to traffic.
		want := 38 * time.Hour
		if ttl > want || ttl < want-time.Minute {
			t.Errorf("salt TTL = %v, want ~%v (expiry anchored to D+2 00:00 UTC)", ttl, want)
		}
		if ttl > saltMaxTTL {
			t.Errorf("salt TTL %v exceeds the %v accepted window", ttl, saltMaxTTL)
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
	if degraded != DegradeNone {
		t.Fatalf("healthy Redis must not degrade, got reason %q", degraded)
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
		if deg1 == DegradeNone || deg2 == DegradeNone {
			t.Fatal("unreachable Redis must report degraded")
		}
		// The reason must survive to the caller: an undifferentiated "degraded"
		// cannot tell a permanent deployment fault from a transient blip.
		if deg1 != DegradeGetFailed {
			t.Errorf("degrade reason = %q, want %q", deg1, DegradeGetFailed)
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

// TestSessionID_InWindowOutOfOrderKeepsOneSession pins the two writes that must
// agree about the watermark, in the one sequence where disagreeing shows.
//
// TestSessionID_OutOfOrderDoesNotFragment covers the MINT path's guard (an event
// far enough back to fall outside the window). This covers its sibling: an event
// that lands INSIDE the window but BEHIND the watermark takes the slide path
// instead, and slideSession must refuse to move the mark backwards.
//
// Mutation-verified — the suite was green against both of:
//   - slideSession writing unconditionally (dropping `if occur.After(last)`)
//   - withinInactivity comparing forward-only (dropping the abs)
//
// Both are the shape an SDK flushing a buffered batch out of order produces, and
// both silently split one visit into several.
func TestSessionID_InWindowOutOfOrderKeepsOneSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return base }
	const did = IDPrefix + "inwindow"

	first, _ := r.SessionID(ctx, "p1", did, "20260720", base)

	// Forward 20min: joins, and advances the watermark to base+20m.
	if got, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(20*time.Minute)); got != first {
		t.Fatalf("20min forward must reuse the session: %q vs %q", got, first)
	}

	// Backward 15min from the watermark, still inside sessionInactivity. Only a
	// symmetric window admits it; a forward-only comparison mints instead.
	if got, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(5*time.Minute)); got != first {
		t.Errorf("event 15min BEFORE last activity is within the window and must stay in the session: %q vs %q", got, first)
	}

	// 25min past the true watermark (base+20m) — inside the window. If the
	// backward event above had been allowed to rewind the mark to base+5m, this
	// would measure a 40min gap and mint a second session.
	if got, _ := r.SessionID(ctx, "p1", did, "20260720", base.Add(45*time.Minute)); got != first {
		t.Errorf("25min after the true watermark must stay in the session: %q vs %q — a backward event rewound the mark", got, first)
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
	// Day derived from the pinned clock, not written beside it: saltForDay rejects
	// any day outside DayOf's window, so a literal day on the real clock expires.
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	day := Day(now.Format(dayFormat))

	winner := make([]byte, saltLen)
	for i := range winner {
		winner[i] = byte(i + 1)
	}
	rd.Client.AddHook(&raceHook{
		rdb:    rd.Client,
		key:    saltKeyPrefix + string(day),
		winner: base64.StdEncoding.EncodeToString(winner),
	})

	r := New(rd.Client)
	r.now = func() time.Time { return now }
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

// sessionRaceHook installs a live session value the first time a GET on the
// session key misses, so the SetArgs(NX, GET) that follows loses the mint race
// and must adopt the value it finds.
//
// This is the multi-pod correctness story the package doc advertises, and the
// branch that implements it had zero coverage: the window is microseconds wide
// in production, so it has to be driven deterministically rather than by racing
// goroutines and hoping. Mirrors raceHook, which does the same for the salt.
type sessionRaceHook struct {
	rdb    *redis.Client
	key    string
	winner string
	once   sync.Once
}

func (h *sessionRaceHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *sessionRaceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *sessionRaceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() != "get" || !errors.Is(err, redis.Nil) {
			return err
		}
		if len(cmd.Args()) < 2 || fmt.Sprint(cmd.Args()[1]) != h.key {
			return err
		}
		h.once.Do(func() { h.rdb.Set(ctx, h.key, h.winner, time.Hour) })
		return err
	}
}

// TestSessionID_LostMintRaceAdoptsPriorSession pins the SETNX-loser branch.
//
// Two pods resolving the same visitor concurrently both see an absent key and
// both try to mint. The loser's SetArgs returns the winner's value instead of
// nil, and it must ADOPT that session rather than keep the one it just
// generated. Without this, every pod that loses the race starts its own session
// for a visitor who is mid-visit — one visit becomes N sessions under any
// multi-replica deploy, and no single-pod test can see it.
func TestSessionID_LostMintRaceAdoptsPriorSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	const did = IDPrefix + "raced"
	const winner = "f47ac10b-58cc-4372-a567-0e02b2c3d998"

	rd.Client.AddHook(&sessionRaceHook{
		rdb: rd.Client,
		key: sessKeyPrefix + "p1:" + did,
		// Live: 5 minutes before this event, well inside sessionInactivity.
		winner: formatSession(winner, base.Add(-5*time.Minute)),
	})

	r := New(rd.Client)
	r.now = func() time.Time { return base }

	got, degraded := r.SessionID(ctx, "p1", did, "20260720", base)
	if degraded != DegradeNone {
		t.Fatalf("losing a mint race is not a degradation, got reason %q", degraded)
	}
	if got != winner {
		t.Errorf("session = %q, want the winner's %q — the loser kept its own session and the visit forked", got, winner)
	}
}

// TestResolver_ConcurrentUse exercises the "Safe for concurrent use" claim on
// the Resolver's doc. Run under -race this is the only thing that checks the
// mutex actually covers every path into the salt map.
func TestResolver_ConcurrentUse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return now }

	const goroutines = 16
	var wg sync.WaitGroup
	ids := make([]string, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Alternate the day so both accepted-window entries are written and
			// read concurrently, and the prune runs while others are reading.
			day := Day("20260720")
			if i%2 == 1 {
				day = "20260719"
			}
			id, err := r.DistinctID(ctx, day, "p1", "203.0.113.7", "Mozilla/5.0")
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			ids[i] = id
			r.SessionID(ctx, "p1", id, day, now)
		}()
	}
	wg.Wait()

	// Same inputs must have produced the same id on every goroutine that used
	// the same day — a torn read of the salt map would show up as a fork.
	for i := 2; i < goroutines; i++ {
		if ids[i] != ids[i%2] {
			t.Errorf("goroutine %d id = %q, want %q — concurrent salt access forked the fleet", i, ids[i], ids[i%2])
		}
	}
}

// failReadsHook makes every GET fail, driving SessionID down the get_failed
// degrade path on every single call — the shape of a persistent fault.
type failReadsHook struct{ err error }

func (h *failReadsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failReadsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *failReadsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}

// TestSessionID_RepeatedDegradeIsLoggedOnce pins that a PERSISTENT session-path
// fault does not emit one ERROR line plus one recorded span exception per event.
//
// Every degrade path here fires per event, and the faults that reach them are by
// construction the ones that affect every event: a read-only replica after
// failover, OOM under noeviction, or `SET NX GET` rejected outright by a Redis
// older than 7.0. At ingest volume that is log-pipeline saturation, exception
// spam, and every other error in the service buried — the counter is the alerting
// signal, the log is not.
//
// The handler already solved exactly this for the salt path with its
// saltFailureLogged per-request latch; the session path had no equivalent because
// its logging lives down here, where "per request" is not a thing this package can
// see. So it is bounded by time per reason instead: the FIRST occurrence is always
// reported, recurrence is throttled, and a new reason is never suppressed by a
// different one.
//
// The returned DegradeReason is deliberately NOT throttled — the handler's
// per-event counter must stay exact.
func TestSessionID_RepeatedDegradeIsLoggedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return base }
	rd.Client.AddHook(&failReadsHook{err: errors.New("connection refused")})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const events = 5
	for i := range events {
		_, reason := r.SessionID(ctx, "p1", IDPrefix+"noisy", "20260720",
			base.Add(time.Duration(i)*time.Second))
		if reason != DegradeGetFailed {
			t.Fatalf("event %d reason = %q, want %q — the per-event signal must survive throttling",
				i, reason, DegradeGetFailed)
		}
	}

	if got := strings.Count(buf.String(), "cookieless session state unavailable"); got != 1 {
		t.Errorf("logged %d times for %d events, want 1 — a persistent fault must not emit "+
			"an ERROR line and a span exception per event", got, events)
	}
}

// failWritesHook lets GET succeed while every SET fails, reproducing the states
// where Redis serves reads but refuses writes: `maxmemory` reached under
// noeviction (SET carries the denyoom flag, GET does not), or a read-only
// replica after a failover.
type failWritesHook struct{ err error }

func (h *failWritesHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failWritesHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failWritesHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}

// TestSessionID_SlideFailureIsReported pins that a watermark that cannot advance
// is surfaced rather than swallowed.
//
// This is the failure mode a plain "is Redis up?" check misses entirely. Reads
// keep working, so the stitch path takes its happy branch and returns the RIGHT
// session id — but the last-activity watermark freezes, and every later event
// measures its gap from the frozen mark until one exceeds sessionInactivity and
// mints. One visit silently becomes several. Before this, slideSession discarded
// the error and both callers reported healthy.
// TestSessionID_CorruptStateIsReported pins that an unparseable stored session is
// distinguished from an ordinary inactivity expiry.
//
// Both take the mint path, so the two were previously indistinguishable — no log,
// no counter, no DegradeReason, only the comment "Present but expired-by-inactivity
// (or corrupt): mint below" naming the ambiguity and doing nothing with it. That
// matters because the failure is silent AND permanent: dashboard_session_rollup is
// keyed by session_id (migration 007), so every event that re-mints against a
// corrupt value writes a session row that cannot be reconciled after the fact. A
// rolling deploy that changes formatSession's encoding, an external writer to
// cookieless:sess:*, or a truncated write all produce it, and all three look
// exactly like healthy traffic.
//
// The returned id is still correct and usable. Like slide_failed, this reports
// state that is wrong behind an answer that is right.
func TestSessionID_CorruptStateIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	r := New(rd.Client)
	r.now = func() time.Time { return base }
	const did = IDPrefix + "corrupt"

	key := sessKeyPrefix + "p1:" + did
	if err := rd.Client.Set(ctx, key, "not-a-session", sessionTTL).Err(); err != nil {
		t.Fatalf("seed corrupt session: %v", err)
	}

	sid, reason := r.SessionID(ctx, "p1", did, "20260720", base)
	if reason != DegradeCorruptState {
		t.Errorf("reason = %q, want %q — a corrupt value must not be reported as a clean mint",
			reason, DegradeCorruptState)
	}
	if sid == "" {
		t.Error("a corrupt stored session must still yield a usable id")
	}

	// The corrupt value must be REPLACED, not left to re-trigger forever: a second
	// event in the same window rejoins the freshly minted session cleanly.
	sid2, reason2 := r.SessionID(ctx, "p1", did, "20260720", base.Add(time.Minute))
	if sid2 != sid {
		t.Errorf("second event = %q, want the minted session %q — corrupt state was not overwritten", sid2, sid)
	}
	if reason2 != DegradeNone {
		t.Errorf("second event reason = %q, want none — the corrupt value should be gone", reason2)
	}
}

func TestSessionID_SlideFailureIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	const did = IDPrefix + "slidefail"

	r := New(rd.Client)
	r.now = func() time.Time { return base }

	// Establish a live session while writes still work.
	first, degraded := r.SessionID(ctx, "p1", did, "20260720", base)
	if degraded != DegradeNone {
		t.Fatalf("setup must not degrade, got %q", degraded)
	}

	rd.Client.AddHook(&failWritesHook{err: errors.New("OOM command not allowed when used memory > 'maxmemory'")})

	// Ten minutes later: still the same session (the READ succeeded), but the
	// slide could not persist and that has to be visible.
	got, reason := r.SessionID(ctx, "p1", did, "20260720", base.Add(10*time.Minute))
	if got != first {
		t.Errorf("session = %q, want %q — a failed slide must not change which session the event joins", got, first)
	}
	if reason != DegradeSlideFailed {
		t.Errorf("degrade reason = %q, want %q — a frozen watermark re-splits the session on later events and must not read as healthy", reason, DegradeSlideFailed)
	}
}
