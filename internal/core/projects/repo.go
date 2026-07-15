package projects

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
	goredis "github.com/redis/go-redis/v9"
)

const (
	privateKeyCachePrefix = "project:prvkey:"
	publicKeyCachePrefix  = "project:pubkey:"
	// apiKeyCacheTTL bounds how long a revoked key keeps authenticating if its
	// invalidation is lost (Redis down, pod killed between the delete and the
	// DEL). It is purely a backstop — not the consistency mechanism: under normal
	// operation the generation-guarded invalidation on every revocation takes
	// effect immediately. Keep it short enough that a leaked key the owner has
	// revoked cannot outlive the incident.
	apiKeyCacheTTL = time.Hour

	// apiKeyGenCachePrefix namespaces the per-token generation counter that makes
	// cache population race-safe (see apiKeyPopulateScript). A reader observes the
	// counter before its DB read and only writes the project back if the counter is
	// unchanged at write time; every invalidation bumps it. Without this, a reader
	// that missed the cache and resolved a still-live key could Set the project
	// back *after* a concurrent DeleteApiKey's invalidation, resurrecting the
	// revoked key until the TTL expired. That race is the likeliest failure here,
	// not a contrived one: a key is revoked precisely when it is in active use, so
	// a lookup in flight at revocation time is near-certain.
	apiKeyGenCachePrefix = "project:keygen:"
	// apiKeyGenCacheTTL bounds growth of the generation counters (one extra Redis
	// key per cached token). It only needs to outlive a single lookup's
	// observe→DB-read→populate window — two indexed reads — so an hour is vast
	// headroom while still letting the counters for revoked keys expire instead of
	// leaking forever.
	apiKeyGenCacheTTL = time.Hour
)

// apiKeyGenCacheKey names a token's generation counter. Keyed by token, like the
// cached rows it guards: a private token is a 64-char digest and a public one a
// 24-char pub_ value, so the two kinds share this namespace without colliding.
func apiKeyGenCacheKey(token string) string {
	return apiKeyGenCachePrefix + token
}

var (
	// apiKeyPopulateScript writes the cached project row (KEYS[1]) only when the
	// token's generation counter (KEYS[2]) still equals the value the caller
	// observed before its DB read (ARGV[2]; "" means the counter was absent). This
	// closes the cache-aside race: a reader that resolved a key which a concurrent
	// DeleteApiKey then revoked and invalidated finds the generation bumped and
	// skips the write, so it can never resurrect the revoked key after
	// invalidation. ARGV[1]=project JSON, ARGV[3]=value TTL in seconds.
	apiKeyPopulateScript = goredis.NewScript(`
local cur = redis.call('GET', KEYS[2])
if cur == false then cur = '' end
if cur == ARGV[2] then
  redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[3])
  return 1
end
return 0
`)

	// apiKeyInvalidateScript bumps one token's generation counter (KEYS[1],
	// refreshing its TTL) and drops both cached rows it could be reached by
	// (KEYS[2] private, KEYS[3] public) atomically. Both prefixes go every time
	// because callers hold tokens, not kinds, and deleting an absent entry costs
	// nothing. The INCR is what makes invalidation race-safe: any in-flight reader
	// that observed the prior generation fails apiKeyPopulateScript's compare and
	// skips its write. ARGV[1]=generation TTL in milliseconds.
	apiKeyInvalidateScript = goredis.NewScript(`
redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('DEL', KEYS[2], KEYS[3])
return 1
`)
)

type Repo struct {
	queries *dbread.Queries
	cache   *goredis.Client
}

func NewRepo(queries *dbread.Queries, cache *goredis.Client) *Repo {
	return &Repo{queries: queries, cache: cache}
}

func (r *Repo) GetProjectByPrivateApiKey(ctx context.Context, privateApiKey string) (dbread.Project, error) {
	// Private keys are stored hashed, so the digest — never the key itself — is
	// what reaches the DB and what the cache entry is keyed by: a dump of either
	// hands an attacker no working credential.
	token := hashKey(privateApiKey)
	cacheKey := privateKeyCachePrefix + token

	if project, ok := r.cachedProject(ctx, cacheKey); ok {
		return project, nil
	}

	// Observed before the DB read so the populate below can detect a revocation
	// that lands during it. An unreadable counter is no baseline at all, so the
	// populate is skipped rather than risked — see observeKeyGen.
	observedGen, canPopulate := r.observeKeyGen(ctx, token)

	project, err := r.queries.GetProjectByPrivateApiKey(ctx, token)
	if err != nil {
		// pgx.ErrNoRows is the expected "invalid API key" miss — caller translates to authn error.
		// Real DB failures (connection, timeout, etc.) log + record at source.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to query project by private api key", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
		return dbread.Project{}, err
	}

	if canPopulate {
		r.cacheProject(ctx, cacheKey, token, observedGen, project)
	}

	return project, nil
}

func (r *Repo) GetProjectByPublicApiKey(ctx context.Context, publicApiKey string) (dbread.Project, error) {
	// A public key is stored whole, so it is its own token.
	token := publicApiKey
	cacheKey := publicKeyCachePrefix + token

	if project, ok := r.cachedProject(ctx, cacheKey); ok {
		return project, nil
	}

	// Observed before the DB read — see GetProjectByPrivateApiKey.
	observedGen, canPopulate := r.observeKeyGen(ctx, token)

	project, err := r.queries.GetProjectByPublicApiKey(ctx, publicApiKey)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to query project by public api key", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
		return dbread.Project{}, err
	}

	if canPopulate {
		r.cacheProject(ctx, cacheKey, token, observedGen, project)
	}

	return project, nil
}

// InvalidateProjectKeys drops the cached project row reached by each of the given
// api_keys tokens and bumps each token's generation counter, atomically
// (apiKeyInvalidateScript). Call it after every successful revocation. The
// generation bump is the race guard: an in-flight lookup that already resolved a
// now-revoked key observes the changed generation and skips its populate, so it
// cannot resurrect the key after this invalidation.
//
// Best-effort by design (mirroring coreorgs.invalidateMemberRole): the DB is the
// source of truth and has already been updated; a failure is logged + recorded and
// apiKeyCacheTTL bounds the residual staleness.
func (r *Repo) InvalidateProjectKeys(ctx context.Context, projectID string, tokens ...string) {
	for _, token := range tokens {
		if err := apiKeyInvalidateScript.Run(ctx, r.cache,
			[]string{apiKeyGenCacheKey(token), privateKeyCachePrefix + token, publicKeyCachePrefix + token},
			apiKeyGenCacheTTL.Milliseconds(),
		).Err(); err != nil {
			// ERROR, not WARN: this is the one project-cache failure with a security
			// consequence — a revoked key keeps authenticating until apiKeyCacheTTL.
			// Read/populate failures stay WARN (they self-heal by falling through to
			// Postgres). The remaining tokens are still attempted: each guards a
			// different key, and a failure on one says nothing about the next.
			//
			// project_id, never the token: enough to find the project whose
			// revocation was lost, without logging a credential.
			slog.ErrorContext(ctx, "failed to invalidate project api key cache", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}
}

// observeKeyGen returns the token's generation counter, captured before the DB read
// so cacheProject's compare-and-set can detect a revocation that lands during it.
// The bool reports whether that baseline is trustworthy enough to populate from.
//
// An absent counter is a real baseline ("", true): the token has never been
// invalidated, and apiKeyPopulateScript is meant to match "" against a still-absent
// counter and cache the row. A failed read is emphatically not that — it is
// "unknown" — and must not borrow the same "" that means absent, because the CAS
// cannot tell the two apart and would treat the unknown as a match. If the Redis
// blip that failed this read also swallowed a concurrent DeleteApiKey's INCR, the
// counter stays absent, the compare passes on recovery, and the reader writes the
// *revoked* key back into a clean cache for apiKeyCacheTTL — the exact resurrection
// the generation counter exists to prevent, and worse than a lost invalidation,
// which at least leaves nothing behind to authenticate with.
//
// So an unreadable counter returns false and the caller skips its populate. The
// whole price is that the next lookup pays a DB round-trip; the cache converges as
// soon as Redis is healthy again.
func (r *Repo) observeKeyGen(ctx context.Context, token string) (string, bool) {
	gen, err := r.cache.Get(ctx, apiKeyGenCacheKey(token)).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return "", true
		}
		slog.WarnContext(ctx, "failed to read api key cache generation", slogx.Error(err))
		return "", false
	}
	return gen, true
}

// cachedProject returns the project cached under cacheKey. A corrupt entry is
// deleted and reported as a miss.
func (r *Repo) cachedProject(ctx context.Context, cacheKey string) (dbread.Project, bool) {
	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil {
		if !errors.Is(err, goredis.Nil) {
			slog.WarnContext(ctx, "failed to get project by api key from cache", slogx.Error(err))
		}
		return dbread.Project{}, false
	}

	var project dbread.Project
	if err := json.Unmarshal(data, &project); err != nil {
		slog.WarnContext(ctx, "failed to unmarshal cached project by api key, deleting corrupt entry", slogx.Error(err))
		if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
			slog.WarnContext(ctx, "failed to delete corrupt cache entry", slogx.Error(err), slog.String("cache_key", cacheKey))
		}
		return dbread.Project{}, false
	}

	return project, true
}

// cacheProject stores project under cacheKey, but only if token's generation is
// still observedGen — the CAS that keeps a lookup racing a concurrent revocation
// from writing the revoked key back (see apiKeyPopulateScript). Best-effort: a
// skip or a failure costs the next lookup a DB round-trip, never a stale row.
func (r *Repo) cacheProject(ctx context.Context, cacheKey, token, observedGen string, project dbread.Project) {
	data, err := json.Marshal(project)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by api key for caching", slogx.Error(err))
		return
	}
	if err := apiKeyPopulateScript.Run(ctx, r.cache,
		[]string{cacheKey, apiKeyGenCacheKey(token)},
		data, observedGen, int(apiKeyCacheTTL.Seconds()),
	).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by api key", slogx.Error(err))
	}
}
