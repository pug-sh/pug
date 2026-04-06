package projects_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
)

func TestProjectsRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	// Seed an org — projects belong to orgs.
	write := dbwrite.New(db.PgW)
	if _, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-repo",
		DisplayName: "Repo Org",
	}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Use the service to create a project — generates proper xid (20-char) IDs and API keys.
	svc := projects.NewService(db.PgRO, db.PgW, nil)
	proj, err := svc.CreateProject(ctx, "org-repo", "Repo Project")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	prvKey := proj.PrivateApiKey
	pubKey := proj.PublicApiKey
	projectID := proj.ID

	queries := dbread.New(db.PgRO)
	repo := projects.NewRepo(queries, rd.Client)

	t.Run("private key cache miss then hit", func(t *testing.T) {
		p1, err := repo.GetProjectByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if strings.TrimSpace(p1.ID) != projectID {
			t.Errorf("project ID = %q, want %q", p1.ID, projectID)
		}

		p2, err := repo.GetProjectByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("second call (cache hit): %v", err)
		}
		if p2.ID != p1.ID {
			t.Errorf("cache hit returned different project ID: %q vs %q", p2.ID, p1.ID)
		}
	})

	t.Run("public key cache miss then hit", func(t *testing.T) {
		p1, err := repo.GetProjectByPublicApiKey(ctx, pubKey)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if strings.TrimSpace(p1.ID) != projectID {
			t.Errorf("project ID = %q, want %q", p1.ID, projectID)
		}

		p2, err := repo.GetProjectByPublicApiKey(ctx, pubKey)
		if err != nil {
			t.Fatalf("second call (cache hit): %v", err)
		}
		if p2.ID != p1.ID {
			t.Errorf("cache hit returned different project ID")
		}
	})

	t.Run("invalidate clears cache", func(t *testing.T) {
		// Populate cache.
		if _, err := repo.GetProjectByPrivateApiKey(ctx, prvKey); err != nil {
			t.Fatalf("populate cache: %v", err)
		}

		prvCacheKey := "project:prvkey:" + prvKey
		pubCacheKey := "project:pubkey:" + pubKey

		exists := rd.Client.Exists(ctx, prvCacheKey).Val()
		if exists != 1 {
			t.Fatal("expected private key cache entry to exist before invalidation")
		}

		repo.InvalidateProjectKeys(ctx, prvKey, pubKey)

		if rd.Client.Exists(ctx, prvCacheKey).Val() != 0 {
			t.Error("expected private key cache entry to be deleted")
		}
		if rd.Client.Exists(ctx, pubCacheKey).Val() != 0 {
			t.Error("expected public key cache entry to be deleted")
		}
	})

	t.Run("corrupt cache falls through to DB", func(t *testing.T) {
		cacheKey := "project:prvkey:" + prvKey
		err := rd.Client.Set(ctx, cacheKey, "not-json{{{", 0).Err()
		if err != nil {
			t.Fatalf("set corrupt cache: %v", err)
		}

		p, err := repo.GetProjectByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("call with corrupt cache: %v", err)
		}
		if strings.TrimSpace(p.ID) != projectID {
			t.Errorf("project ID = %q, want %q", p.ID, projectID)
		}

		raw, err := rd.Client.Get(ctx, cacheKey).Result()
		if err != nil {
			t.Fatalf("get cache after recovery: %v", err)
		}
		if raw == "not-json{{{" {
			t.Error("corrupt cache entry was not replaced")
		}
	})

	t.Run("nonexistent key returns error", func(t *testing.T) {
		if _, err := repo.GetProjectByPrivateApiKey(ctx, "prv_doesnotexist0000000"); err == nil {
			t.Fatal("expected error for nonexistent key, got nil")
		}
	})
}
