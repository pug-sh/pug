package email_test

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestOrgProviderRepoCacheHitAndMiss(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-cachehit", DisplayName: "Cache Hit"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID:            org.ID,
		Kind:             string(coreemail.ProviderKindResend),
		FromAddress:      "ops@acme.com",
		ReplyTo:          postgres.NewOptionalText("support@acme.com"),
		SecretCiphertext: []byte("ciphertext-bytes"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	repo := coreemail.NewOrgProviderRepo(dbread.New(db.PgRO), rds.Client)

	// First call → miss → populates cache.
	entry, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get miss: %v", err)
	}
	if !entry.Present || entry.FromAddress != "ops@acme.com" || string(entry.SecretCiphertext) != "ciphertext-bytes" {
		t.Fatalf("unexpected entry on miss: %+v", entry)
	}

	// Delete from DB, then call again — cache should still serve it.
	if _, err := write.DeleteOrgEmailProvider(ctx, org.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entry, err = repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get hit: %v", err)
	}
	if !entry.Present {
		t.Fatal("expected cache hit, got miss after deletion")
	}

	// Invalidate, then call — should now reflect deletion.
	if err := repo.Invalidate(ctx, org.ID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	entry, err = repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if entry.Present {
		t.Fatal("expected absent entry after invalidate")
	}
}

// TestOrgProviderRepoCorruptCacheEntry pins the corrupt-cache recovery path:
// if a cached blob fails to unmarshal, Get must delete the key and fall back
// to the DB. A regression that silently returned the zero-value entry would
// serve "no provider" forever for that org.
func TestOrgProviderRepoCorruptCacheEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-corrupt", DisplayName: "Corrupt"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID:            org.ID,
		Kind:             string(coreemail.ProviderKindResend),
		FromAddress:      "ops@acme.com",
		ReplyTo:          postgres.NewOptionalText(""),
		SecretCiphertext: []byte("ciphertext-bytes"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	cacheKey := "email:org_provider:" + org.ID
	if err := rds.Client.Set(ctx, cacheKey, []byte("not-json"), 0).Err(); err != nil {
		t.Fatalf("seed corrupt cache: %v", err)
	}

	repo := coreemail.NewOrgProviderRepo(dbread.New(db.PgRO), rds.Client)
	entry, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get on corrupt cache: %v", err)
	}
	if !entry.Present || entry.FromAddress != "ops@acme.com" {
		t.Fatalf("expected DB-backed entry on corrupt cache, got %+v", entry)
	}
	// After Get, the corrupt key should have been replaced by the
	// re-marshaled DB result (or at minimum deleted). Either way it should
	// no longer be the literal "not-json" string.
	raw, err := rds.Client.Get(ctx, cacheKey).Bytes()
	if err == nil && string(raw) == "not-json" {
		t.Fatal("expected corrupt cache entry to be replaced, still 'not-json'")
	}
}

// TestOrgProviderRepoIsolatesAcrossOrgs pins that the cache key carries the
// orgID through correctly: two orgs with different providers must each
// resolve to their own entry. A regression that built the cache key from a
// captured constant — or that returned one org's cached entry for another's
// query — would cause cross-tenant provider bleed and not fail today.
//
// After deleting org-a's DB row, we ask for org-b: org-b's Get must still
// return its own (non-deleted) entry. This is stronger than "different
// entries on first call" because it pins that hits on different keys can't
// shadow each other.
func TestOrgProviderRepoIsolatesAcrossOrgs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	orgA, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "iso-a", DisplayName: "A"})
	if err != nil {
		t.Fatalf("CreateOrg A: %v", err)
	}
	orgB, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "iso-b", DisplayName: "B"})
	if err != nil {
		t.Fatalf("CreateOrg B: %v", err)
	}
	if _, err := write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID: orgA.ID, Kind: string(coreemail.ProviderKindResend),
		FromAddress: "ops@a.com", ReplyTo: postgres.NewOptionalText(""),
		SecretCiphertext: []byte("ct-a"),
	}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}
	if _, err := write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID: orgB.ID, Kind: string(coreemail.ProviderKindResend),
		FromAddress: "ops@b.com", ReplyTo: postgres.NewOptionalText(""),
		SecretCiphertext: []byte("ct-b"),
	}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}

	repo := coreemail.NewOrgProviderRepo(dbread.New(db.PgRO), rds.Client)

	// Warm cache for A.
	entryA, err := repo.Get(ctx, orgA.ID)
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if entryA.FromAddress != "ops@a.com" || string(entryA.SecretCiphertext) != "ct-a" {
		t.Fatalf("A entry wrong: %+v", entryA)
	}

	// Delete A's DB row. B's cache miss must still surface B's data (not A's,
	// not absent).
	if _, err := write.DeleteOrgEmailProvider(ctx, orgA.ID); err != nil {
		t.Fatalf("Delete A: %v", err)
	}
	entryB, err := repo.Get(ctx, orgB.ID)
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if entryB.FromAddress != "ops@b.com" || string(entryB.SecretCiphertext) != "ct-b" {
		t.Fatalf("B entry wrong (cross-tenant bleed?): %+v", entryB)
	}

	// A's cache still warm from the first Get; invalidate then assert A now
	// reflects DB-side deletion without affecting B.
	if err := repo.Invalidate(ctx, orgA.ID); err != nil {
		t.Fatalf("Invalidate A: %v", err)
	}
	entryA2, err := repo.Get(ctx, orgA.ID)
	if err != nil {
		t.Fatalf("Get A after invalidate: %v", err)
	}
	if entryA2.Present {
		t.Fatal("expected A absent after delete+invalidate")
	}
	entryB2, err := repo.Get(ctx, orgB.ID)
	if err != nil {
		t.Fatalf("Get B again: %v", err)
	}
	if !entryB2.Present || entryB2.FromAddress != "ops@b.com" {
		t.Fatalf("B unexpectedly affected by A's invalidate: %+v", entryB2)
	}
}

// TestOrgProviderRepoCacheDownFallsThroughToDB pins the design contract that
// Redis is best-effort. When the cache is unreachable, Get must still return
// the DB row rather than blocking forever or returning a misleading absent.
// We construct a goredis.Client pointed at a closed port to simulate a
// reachable-but-broken cache (DNS resolves but connection refuses).
func TestOrgProviderRepoCacheDownFallsThroughToDB(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "cache-down", DisplayName: "Cache Down"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID: org.ID, Kind: string(coreemail.ProviderKindResend),
		FromAddress: "ops@acme.com", ReplyTo: postgres.NewOptionalText(""),
		SecretCiphertext: []byte("ct-cd"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Use port 1 (typically not listening) with aggressive timeouts so the
	// test doesn't drag if the connection refused is slow.
	broken := goredis.NewClient(&goredis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = broken.Close() })

	repo := coreemail.NewOrgProviderRepo(dbread.New(db.PgRO), broken)

	start := time.Now()
	entry, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get with cache down: %v", err)
	}
	if !entry.Present || entry.FromAddress != "ops@acme.com" || string(entry.SecretCiphertext) != "ct-cd" {
		t.Fatalf("expected DB-backed entry, got %+v", entry)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Get blocked too long on cache-down: %v", elapsed)
	}
}

func TestOrgProviderRepoNegativeCache(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	repo := coreemail.NewOrgProviderRepo(dbread.New(db.PgRO), rds.Client)
	entry, err := repo.Get(ctx, "org-missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Present {
		t.Fatal("expected absent")
	}

	// Cached as negative entry: same key, JSON has `"present":false`.
	cacheKey := "email:org_provider:org-missing"
	raw, err := rds.Client.Get(ctx, cacheKey).Bytes()
	if err != nil {
		t.Fatalf("expected cache key set, got err %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("expected cache payload")
	}
}
