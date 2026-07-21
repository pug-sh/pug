package cookieless

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// sessionNamespace scopes the deterministic fallback UUIDs (v5) so they can
// never collide with any other UUID namespace in the system.
var sessionNamespace = uuid.MustParse("5a1e8e1e-7d5a-4f3b-9c2e-0c00c1e55e55")

// SessionID returns the stitched session for one cookieless visitor event:
// reuse the Redis-held session while the gap from its last activity is within
// sessionInactivity (computed on event time — the Redis TTL is garbage
// collection, not session semantics), else mint. degraded=true means Redis was
// unreachable and the id is the deterministic one-session-per-visitor-day
// fallback: data still flows, sessions coarsen for the outage window.
//
// Two pods resolving the same visitor concurrently can race the mint; SETNX+GET
// adopts the winner when the loser observes it, and the residual last-write-wins
// overlap splits at most one session — bounded, accepted.
func (r *Resolver) SessionID(ctx context.Context, projectID, distinctID, day string, occur time.Time) (string, bool) {
	key := sessKeyPrefix + projectID + ":" + distinctID

	val, err := r.rdb.Get(ctx, key).Result()
	switch {
	case err == nil:
		if sid, last, ok := parseSession(val); ok && withinInactivity(occur, last) {
			r.slideSession(ctx, key, sid, last, occur)
			return sid, false
		}
		// Present but expired-by-inactivity (or corrupt): mint below.
	case !errors.Is(err, redis.Nil):
		return r.fallbackSessionID(distinctID, day), true
	}

	fresh := uuid.NewString()
	prior, err := r.rdb.SetArgs(ctx, key, formatSession(fresh, occur), redis.SetArgs{
		Mode: "NX", Get: true, TTL: sessionTTL,
	}).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return fresh, false // key was absent; our mint landed
	case err != nil:
		return r.fallbackSessionID(distinctID, day), true
	}
	// Key existed (another pod minted, or GET raced an expiry edge): adopt the
	// prior session when it is still live, else overwrite.
	if sid, last, ok := parseSession(prior); ok && withinInactivity(occur, last) {
		r.slideSession(ctx, key, sid, last, occur)
		return sid, false
	}
	// The event falls outside the stored session's window, so it gets its own
	// session — but the DIRECTION decides whether it may become the new watermark.
	// A backward event (late flush, replayed batch, backfill) that recorded its own
	// time would rewind last-activity, and every subsequent event already stitched
	// to the live session would then measure against the rewound mark and re-mint:
	// two events with the same occur_time end up in different sessions. This is the
	// same monotonicity slideSession enforces at its own write; the two writes must
	// agree. Pinned by TestSessionID_OutOfOrderDoesNotFragment.
	if _, last, ok := parseSession(prior); ok && !occur.After(last) {
		return r.staleSessionID(distinctID, day, occur), false
	}
	if err := r.rdb.Set(ctx, key, formatSession(fresh, occur), sessionTTL).Err(); err != nil {
		return r.fallbackSessionID(distinctID, day), true
	}
	return fresh, false
}

// staleSessionID names the session for an event that arrived after the visitor
// had already moved on — it landed more than sessionInactivity BEFORE the stored
// last-activity, so it cannot join the live session and must not disturb it.
//
// No Redis state is written for these events, so whatever this returns is the
// only thing binding two stranded events together. That makes it a real choice
// about how late-arriving data is sessionized, not an implementation detail.
func (r *Resolver) staleSessionID(distinctID, day string, occur time.Time) string {
	// TODO(session-semantics): decide how stranded events group. See the three
	// candidate models discussed alongside this change:
	//   - per-event   : uuid.NewString() — never collides, but two stranded events
	//                   one minute apart become two sessions (over-fragments).
	//   - per-day     : r.fallbackSessionID(distinctID, day) — all stranded events
	//                   in a visitor-day collapse to one, idempotent under replay,
	//                   but indistinguishable from the Redis-outage fallback.
	//   - per-window  : deterministic over occur's sessionInactivity bucket —
	//                   faithful and replay-stable, but a third minting scheme.
	return uuid.NewString()
}

// slideSession advances last-activity monotonically; best-effort (a lost slide
// costs one over-eager session split, never data).
func (r *Resolver) slideSession(ctx context.Context, key, sid string, last, occur time.Time) {
	if occur.After(last) {
		_ = r.rdb.Set(ctx, key, formatSession(sid, occur), sessionTTL).Err()
	}
}

// withinInactivity treats the gap symmetrically: batches flush out of order,
// and an event slightly BEFORE the recorded last activity is the same session.
func withinInactivity(occur, last time.Time) bool {
	gap := occur.Sub(last)
	if gap < 0 {
		gap = -gap
	}
	return gap <= sessionInactivity
}

func (r *Resolver) fallbackSessionID(distinctID, day string) string {
	return uuid.NewSHA1(sessionNamespace, []byte(distinctID+"|"+day)).String()
}

func formatSession(sid string, last time.Time) string {
	return sid + "|" + strconv.FormatInt(last.UnixMilli(), 10)
}

func parseSession(val string) (sid string, last time.Time, ok bool) {
	sid, ms, found := strings.Cut(val, "|")
	if !found {
		return "", time.Time{}, false
	}
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || sid == "" {
		return "", time.Time{}, false
	}
	return sid, time.UnixMilli(n).UTC(), true
}
