package email_test

import (
	"context"
	"testing"

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
	repo.Invalidate(ctx, org.ID)
	entry, err = repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if entry.Present {
		t.Fatal("expected absent entry after invalidate")
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
