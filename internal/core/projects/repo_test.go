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

	// Seed a customer.
	write := dbwrite.New(db.PgW)
	_, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-repo", Email: "repo@test.com", DisplayName: "Repo Test", PasswordHash: "hash", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	// Use the service to create a project — generates proper xid (20-char) IDs and API keys.
	svc := projects.NewService(db.PgRO, db.PgW, nil)
	proj, err := svc.CreateProject(ctx, "cust-repo", "Repo Project")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	prvKey := proj.PrivateApiKey
	pubKey := proj.PublicApiKey
	projectID := proj.ID

	queries := dbread.New(db.PgRO)
	repo := projects.NewRepo(queries, rd.Client)

	t.Run("private key cache miss then hit", func(t *testing.T) {
		row1, err := repo.GetProjectAndCustomerByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if strings.TrimSpace(row1.Project.ID) != projectID {
			t.Errorf("project ID = %q, want %q", row1.Project.ID, projectID)
		}

		row2, err := repo.GetProjectAndCustomerByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("second call (cache hit): %v", err)
		}
		if row2.Project.ID != row1.Project.ID {
			t.Errorf("cache hit returned different project ID: %q vs %q", row2.Project.ID, row1.Project.ID)
		}
		if row2.Customer.ID != row1.Customer.ID {
			t.Errorf("cache hit returned different customer ID: %q vs %q", row2.Customer.ID, row1.Customer.ID)
		}
	})

	t.Run("public key cache miss then hit", func(t *testing.T) {
		row1, err := repo.GetProjectAndCustomerByPublicApiKey(ctx, pubKey)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if strings.TrimSpace(row1.Project.ID) != projectID {
			t.Errorf("project ID = %q, want %q", row1.Project.ID, projectID)
		}

		row2, err := repo.GetProjectAndCustomerByPublicApiKey(ctx, pubKey)
		if err != nil {
			t.Fatalf("second call (cache hit): %v", err)
		}
		if row2.Project.ID != row1.Project.ID {
			t.Errorf("cache hit returned different project ID")
		}
	})

	t.Run("invalidate clears cache", func(t *testing.T) {
		// Populate cache.
		_, err := repo.GetProjectAndCustomerByPrivateApiKey(ctx, prvKey)
		if err != nil {
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

		row, err := repo.GetProjectAndCustomerByPrivateApiKey(ctx, prvKey)
		if err != nil {
			t.Fatalf("call with corrupt cache: %v", err)
		}
		if strings.TrimSpace(row.Project.ID) != projectID {
			t.Errorf("project ID = %q, want %q", row.Project.ID, projectID)
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
		_, err := repo.GetProjectAndCustomerByPrivateApiKey(ctx, "prv_doesnotexist0000000")
		if err == nil {
			t.Fatal("expected error for nonexistent key, got nil")
		}
	})
}
