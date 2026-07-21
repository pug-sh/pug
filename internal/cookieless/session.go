package cookieless

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// sessionNamespace scopes the deterministic fallback UUIDs (v5) so they can
// never collide with any other UUID namespace in the system.
var sessionNamespace = uuid.MustParse("5a1e8e1e-7d5a-4f3b-9c2e-0c00c1e55e55")

// DegradeReason names why a session could not be stitched from Redis state.
// DegradeNone means the returned id is fully stitched.
//
// It exists because the bool it replaced could not say WHY. Every degrade path
// here is a Redis failure, and the failures need different responses — a
// syntax error (Redis < 7.0 rejecting SET NX GET) is a permanent deployment
// fault, a timeout is transient, and a write-only failure (OOM, READONLY
// replica) leaves reads working so nothing else looks wrong. An operator
// watching one undifferentiated counter cannot tell those apart.
type DegradeReason string

const (
	DegradeNone DegradeReason = ""
	// DegradeGetFailed: the session key could not be read.
	DegradeGetFailed DegradeReason = "get_failed"
	// DegradeMintFailed: the SET NX GET that mints a session failed. On Redis
	// < 7.0 this is `ERR syntax error` on EVERY event — NX and GET were only
	// permitted together from 7.0 — and the key is never created, so it repeats
	// forever while the salt path keeps working.
	DegradeMintFailed DegradeReason = "mint_failed"
	// DegradeWriteFailed: the session could not be persisted after minting.
	DegradeWriteFailed DegradeReason = "write_failed"
	// DegradeSlideFailed: the session is correct but its last-activity watermark
	// could not advance. Distinct from the others because the returned id is
	// still the right one — only the state behind it is stale.
	DegradeSlideFailed DegradeReason = "slide_failed"
)

// SessionID returns the stitched session for one cookieless visitor event:
// reuse the Redis-held session while the gap from its last activity is within
// sessionInactivity (computed on event time — the Redis TTL is garbage
// collection, not session semantics), else mint.
//
// The returned id is ALWAYS usable. A non-empty DegradeReason means it is the
// coarse one-session-per-visitor-day fallback (or, for slide_failed, a correct
// id over a stale watermark): data still flows, sessions coarsen for the
// outage window.
//
// Two pods resolving the same visitor concurrently can race the mint; SETNX+GET
// adopts the winner when the loser observes it, and the residual last-write-wins
// overlap splits at most one session — bounded, accepted.
func (r *Resolver) SessionID(ctx context.Context, projectID, distinctID string, day Day, occur time.Time) (string, DegradeReason) {
	key := sessKeyPrefix + projectID + ":" + distinctID

	val, err := r.rdb.Get(ctx, key).Result()
	switch {
	case err == nil:
		if sid, last, ok := parseSession(val); ok && withinInactivity(occur, last) {
			return sid, r.slideSession(ctx, key, sid, last, occur)
		}
		// Present but expired-by-inactivity (or corrupt): mint below.
	case !errors.Is(err, redis.Nil):
		return r.degrade(ctx, DegradeGetFailed, err, distinctID, day)
	}

	fresh := uuid.NewString()
	prior, err := r.rdb.SetArgs(ctx, key, formatSession(fresh, occur), redis.SetArgs{
		Mode: "NX", Get: true, TTL: sessionTTL,
	}).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return fresh, DegradeNone // key was absent; our mint landed
	case err != nil:
		return r.degrade(ctx, DegradeMintFailed, err, distinctID, day)
	}
	// Key existed (another pod minted, or GET raced an expiry edge): adopt the
	// prior session when it is still live, else overwrite.
	if sid, last, ok := parseSession(prior); ok && withinInactivity(occur, last) {
		return sid, r.slideSession(ctx, key, sid, last, occur)
	}
	// The event falls outside the stored session's window, so it gets its own
	// session — but the DIRECTION decides whether it may become the new watermark.
	// A backward event (late flush, replayed batch, backfill) that recorded its own
	// time would rewind last-activity, and every subsequent event already stitched
	// to the live session would then measure against the rewound mark and re-mint:
	// two events with the same occur_time end up in different sessions. This is the
	// same monotonicity slideSession enforces at its own write; the two writes must
	// agree — TestSessionID_OutOfOrderDoesNotFragment pins this one,
	// TestSessionID_InWindowOutOfOrderKeepsOneSession pins slideSession's.
	if _, last, ok := parseSession(prior); ok && !occur.After(last) {
		return r.staleSessionID(distinctID, day, occur), DegradeNone
	}
	if err := r.rdb.Set(ctx, key, formatSession(fresh, occur), sessionTTL).Err(); err != nil {
		return r.degrade(ctx, DegradeWriteFailed, err, distinctID, day)
	}
	return fresh, DegradeNone
}

// degrade records a Redis failure and returns the coarse day fallback.
//
// This package detects these errors, so per the telemetry convention it is the
// layer that must log and record them — the handler only labels its counter.
// Returning just a reason would throw away the underlying error string, which is
// the one thing that distinguishes a permanent deployment fault from a blip.
func (r *Resolver) degrade(ctx context.Context, reason DegradeReason, err error, distinctID string, day Day) (string, DegradeReason) {
	slog.ErrorContext(ctx, "cookieless session state unavailable, falling back to the visitor-day session",
		slogx.Error(err), slog.String("reason", string(reason)))
	telemetry.RecordError(ctx, err)
	return r.fallbackSessionID(distinctID, day), reason
}

// staleSessionID names the session for an event that arrived after the visitor
// had already moved on — it landed more than sessionInactivity BEFORE the stored
// last-activity, so it cannot join the live session and must not disturb it.
//
// No Redis state is written for these events, so this return value is the only
// thing binding two stranded events together. It groups them by the
// sessionInactivity-sized window occur falls in: two stranded events a minute
// apart share a session, two an hour apart do not — the same rule the live path
// applies, just computed from occur alone because there is no watermark to
// measure against.
//
// Determinism is the load-bearing property, not the grouping. BatchCreate is
// client-retryable and dashboard_session_rollup is keyed by session_id
// (migration 007), so a random id per call would write a fresh session row on
// every retry of the same batch — permanently inflating session counts with no
// way to reconcile after the fact. Hashing occur's window instead makes a
// replayed batch resolve to the ids it resolved to the first time.
//
// Namespaced apart from fallbackSessionID by the "|w" component, so a stranded
// session can never be confused with the Redis-outage day fallback.
// Pinned by TestStaleSessionID_PerWindow.
func (r *Resolver) staleSessionID(distinctID string, day Day, occur time.Time) string {
	window := occur.UTC().Truncate(sessionInactivity).UnixMilli()
	return uuid.NewSHA1(sessionNamespace,
		[]byte(distinctID+"|"+string(day)+"|w"+strconv.FormatInt(window, 10))).String()
}

// slideSession advances last-activity, and only ever forwards: a backward write
// would rewind the watermark and re-split the session on the next event.
//
// A failed slide is not cosmetic. Reads and writes fail independently — under
// `maxmemory` with noeviction, or against a read-only replica after failover,
// GET keeps serving while SET is refused — so the stitch path looks healthy
// while the watermark freezes. Every later event then measures its gap from the
// frozen mark and eventually mints, splitting one visit repeatedly. Report it.
func (r *Resolver) slideSession(ctx context.Context, key, sid string, last, occur time.Time) DegradeReason {
	if !occur.After(last) {
		return DegradeNone
	}
	if err := r.rdb.Set(ctx, key, formatSession(sid, occur), sessionTTL).Err(); err != nil {
		slog.ErrorContext(ctx, "cookieless session watermark could not advance",
			slogx.Error(err), slog.String("reason", string(DegradeSlideFailed)))
		telemetry.RecordError(ctx, err)
		return DegradeSlideFailed
	}
	return DegradeNone
}

// withinInactivity treats the gap symmetrically: batches flush out of order,
// and an event slightly BEFORE the recorded last activity is the same session.
// Pinned by TestSessionID_InWindowOutOfOrderKeepsOneSession.
func withinInactivity(occur, last time.Time) bool {
	gap := occur.Sub(last)
	if gap < 0 {
		gap = -gap
	}
	return gap <= sessionInactivity
}

func (r *Resolver) fallbackSessionID(distinctID string, day Day) string {
	return uuid.NewSHA1(sessionNamespace, []byte(distinctID+"|"+string(day))).String()
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
