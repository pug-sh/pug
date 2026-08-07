package projects_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// sha256Hex mirrors the digest the repo stores and caches private keys under.
// Computed independently of the implementation on purpose: these tests pin the
// on-the-wire format, not whatever the package happens to do.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

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

	queries := dbread.New(db.PgRO)
	repo := projects.NewRepo(queries, rd.Client)

	// Use the service to create a project — generates proper xid (20-char) IDs and
	// its starter public key. A private key is created explicitly, as a user would.
	svc := projects.NewService(db.PgRO, db.PgW, repo)
	proj, err := svc.CreateProject(ctx, "org-repo", "Repo Project", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projectID := proj.ID

	// The starter public key lives only in api_keys — a project row carries none.
	// It is stored whole, so its token is the key itself.
	starter, err := svc.ListApiKeys(ctx, projectID)
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(starter) != 1 {
		t.Fatalf("got %d starter keys, want 1", len(starter))
	}
	pubKey := starter[0].Token

	created, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "test")
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	prvKey := created.RawKey

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

	// The whole point of hashing: a private key must not be recoverable from
	// anything we store — Redis included.
	t.Run("private key is cached under its digest, never its plaintext", func(t *testing.T) {
		if _, err := repo.GetProjectByPrivateApiKey(ctx, prvKey); err != nil {
			t.Fatalf("populate cache: %v", err)
		}

		if rd.Client.Exists(ctx, "project:prvkey:"+sha256Hex(prvKey)).Val() != 1 {
			t.Error("expected the private key's cache entry to be keyed by its digest")
		}
		if rd.Client.Exists(ctx, "project:prvkey:"+prvKey).Val() != 0 {
			t.Error("private key cached under its plaintext")
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
		// Populate both entries.
		if _, err := repo.GetProjectByPrivateApiKey(ctx, prvKey); err != nil {
			t.Fatalf("populate private cache: %v", err)
		}
		if _, err := repo.GetProjectByPublicApiKey(ctx, pubKey); err != nil {
			t.Fatalf("populate public cache: %v", err)
		}

		prvCacheKey := "project:prvkey:" + sha256Hex(prvKey)
		pubCacheKey := "project:pubkey:" + pubKey

		if rd.Client.Exists(ctx, prvCacheKey).Val() != 1 {
			t.Fatal("expected private key cache entry to exist before invalidation")
		}

		// Callers pass tokens — the stored lookup values, not the raw keys. The
		// project id rides along for the log line a failure emits, not the lookup.
		repo.InvalidateProjectKeys(ctx, projectID, sha256Hex(prvKey), pubKey)

		if rd.Client.Exists(ctx, prvCacheKey).Val() != 0 {
			t.Error("expected private key cache entry to be deleted")
		}
		if rd.Client.Exists(ctx, pubCacheKey).Val() != 0 {
			t.Error("expected public key cache entry to be deleted")
		}
	})

	t.Run("corrupt cache falls through to DB", func(t *testing.T) {
		cacheKey := "project:prvkey:" + sha256Hex(prvKey)
		if err := rd.Client.Set(ctx, cacheKey, "not-json{{{", 0).Err(); err != nil {
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
