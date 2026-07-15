package projects

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
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
	observedGen := repo.observeKeyGen(ctx, token)

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

	observedGen := repo.observeKeyGen(ctx, token)
	repo.cacheProject(ctx, cacheKey, token, observedGen, dbread.Project{ID: "proj-quiet"})

	if n := rd.Client.Exists(ctx, cacheKey).Val(); n != 1 {
		t.Fatal("populate skipped with no invalidation racing it; a live key would never cache")
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
	keptGen := repo.observeKeyGen(ctx, kept)

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
