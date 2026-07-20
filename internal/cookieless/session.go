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
	if err := r.rdb.Set(ctx, key, formatSession(fresh, occur), sessionTTL).Err(); err != nil {
		return r.fallbackSessionID(distinctID, day), true
	}
	return fresh, false
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
