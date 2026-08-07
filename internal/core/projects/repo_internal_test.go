package projects

import (
	"context"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
	goredis "github.com/redis/go-redis/v9"
)

// newCacheRaceRepo builds a Repo with no DB handle: these tests drive the cache
// path directly through the same helpers GetProjectBy*ApiKey uses, so no query
// ever runs. Internal rather than in projects_test because the interleaving being
// pinned is only reachable one call at a time — the exported lookup does observe,
// read and populate in a single hop, with nowhere to land a revocation in between.
func newCacheRaceRepo(t *testing.T) (*Repo, *testutil.TestRedis) {
	t.Helper()
	rd := testutil.SetupRedis(t)
	return NewRepo(nil, rd.Client), rd
}

// TestCacheProjectSkipsPopulateAfterConcurrentInvalidation drives the cache-aside
// race by hand, in the order it interleaves in production: a lookup misses the
// cache and resolves a still-live key, a concurrent DeleteApiKey revokes and
// invalidates it, and only then does the lookup write its now-stale row back.
//
// Without the generation CAS that write lands and the revoked key keeps
// authenticating from cache until apiKeyCacheTTL — the failure the generation
// counter exists to prevent, and the likely one, since a key is revoked precisely
// when it is in active use.
func TestCacheProjectSkipsPopulateAfterConcurrentInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	repo, rd := newCacheRaceRepo(t)
	ctx := context.Background()

	const token = "0ff1ce0ff1ce0ff1ce"
	cacheKey := privateKeyCachePrefix + token
	project := dbread.Project{ID: "proj-race"}

	// 1. The lookup observes the generation, then reads the project from the DB.
	observedGen, _ := repo.observeKeyGen(ctx, token)

	// 2. A revocation lands in that window: the cached row is dropped and the
	//    token's generation bumps.
	repo.InvalidateProjectKeys(ctx, project.ID, token)

	// 3. The lookup, still holding the row it read before the revocation, populates.
	repo.cacheProject(ctx, cacheKey, token, observedGen, project)

	if n := rd.Client.Exists(ctx, cacheKey).Val(); n != 0 {
		t.Fatal("populate wrote a revoked project back into the cache after its invalidation: the generation CAS did not hold, so the key would keep authenticating until apiKeyCacheTTL")
	}
}

// TestCacheProjectPopulatesWhenGenerationIsUnchanged is the control for the test
// above. Without it a CAS that never writes at all would pass the race test while
// silently costing every lookup a DB round-trip.
func TestCacheProjectPopulatesWhenGenerationIsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	repo, rd := newCacheRaceRepo(t)
	ctx := context.Background()

	const token = "d00dfeedd00dfeedd00d"
	cacheKey := privateKeyCachePrefix + token

	observedGen, _ := repo.observeKeyGen(ctx, token)
	repo.cacheProject(ctx, cacheKey, token, observedGen, dbread.Project{ID: "proj-quiet"})

	if n := rd.Client.Exists(ctx, cacheKey).Val(); n != 1 {
		t.Fatal("populate skipped with no invalidation racing it; a live key would never cache")
	}
}

// deadRedis returns a client pointing at a port nothing listens on, so every command
// fails with a connection error rather than goredis.Nil. That is what a Redis blip
// looks like to observeKeyGen, and it is the one case the generation value alone
// cannot express: a failed read has no value to report, and "" already means absent.
func deadRedis(t *testing.T) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{
		Addr:        "127.0.0.1:1", // nothing listens here; connections are refused outright
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1, // fail fast: the test wants the error, not go-redis's resilience
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestPopulateSkippedWhenOutageHidGenerationAndInvalidation stages the interleaving
// the CAS cannot detect for itself, in production order: Redis is unreachable when a
// lookup observes the counter, so it gets no baseline; the concurrent DeleteApiKey's
// INCR hits that same dead Redis and is lost, leaving the counter absent; Redis
// recovers before the lookup populates.
//
// If an unreadable counter is reported as merely absent, the compare passes against
// the still-absent counter and the lookup writes the revoked project into a clean
// cache for apiKeyCacheTTL. That is worse than the lost invalidation it rides in on:
// the invalidation had nothing to drop, so declining to populate leaves the next
// lookup to fall through to Postgres and reject the key correctly.
func TestPopulateSkippedWhenOutageHidGenerationAndInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	healthy, rd := newCacheRaceRepo(t)
	ctx := context.Background()

	const token = "deadbeefdeadbeefdead"
	cacheKey := privateKeyCachePrefix + token

	// 1. The lookup observes while Redis is unreachable, and resolves a still-live
	//    key from Postgres.
	down := NewRepo(nil, deadRedis(t))
	observedGen, canPopulate := down.observeKeyGen(ctx, token)

	// 2. The revocation lands in that window, but its INCR is swallowed by the same
	//    outage — modelled by the counter simply never being written.

	// 3. Redis is healthy again by the time the lookup would populate. The caller's
	//    guard is the only thing that can decline the write here, because the value
	//    it holds is indistinguishable from a legitimate absent baseline.
	if canPopulate {
		healthy.cacheProject(ctx, cacheKey, token, observedGen, dbread.Project{ID: "proj-revoked"})
	}

	if n := rd.Client.Exists(ctx, cacheKey).Val(); n != 0 {
		t.Fatal("a lookup that could not read the generation cached its project anyway: with the concurrent revocation's INCR lost to the same outage, the compare passes against the still-absent counter and the revoked key keeps authenticating from cache until apiKeyCacheTTL")
	}
}

// TestObserveKeyGenBaselinesOnAbsentGeneration is the control for the test above: an
// absent counter is a real baseline and must stay usable. Refusing it would be safe
// but would mean a token that has never been revoked could never be cached, quietly
// costing every lookup a DB round-trip.
func TestObserveKeyGenBaselinesOnAbsentGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	repo, _ := newCacheRaceRepo(t)

	gen, canPopulate := repo.observeKeyGen(context.Background(), "cafebabecafebabecafe")
	if !canPopulate {
		t.Fatal("an absent generation was refused as a baseline; a key that has never been revoked would never cache")
	}
	if gen != "" {
		t.Errorf("expected an absent generation to observe as %q, got %q", "", gen)
	}
}

// TestInvalidateProjectKeysBumpsGenerationPerToken pins the guard's granularity:
// revoking one key must not make a project's other keys uncacheable. Both
// generations move independently, and only the revoked token's rows are dropped.
func TestInvalidateProjectKeysBumpsGenerationPerToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	repo, rd := newCacheRaceRepo(t)
	ctx := context.Background()

	const revoked, kept = "aaaa1111", "bbbb2222"

	// A lookup for the key that is about to survive observes its generation first.
	keptGen, _ := repo.observeKeyGen(ctx, kept)

	repo.InvalidateProjectKeys(ctx, "proj-multi", revoked)

	// The surviving key's generation is untouched, so its populate still lands.
	repo.cacheProject(ctx, privateKeyCachePrefix+kept, kept, keptGen, dbread.Project{ID: "proj-multi"})
	if n := rd.Client.Exists(ctx, privateKeyCachePrefix+kept).Val(); n != 1 {
		t.Error("revoking one key blocked another key's populate; the generation is not per-token")
	}

	if n := rd.Client.Exists(ctx, apiKeyGenCacheKey(revoked)).Val(); n != 1 {
		t.Error("expected the revoked token's generation counter to exist after invalidation")
	}
}
