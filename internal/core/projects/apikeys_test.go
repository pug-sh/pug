package projects_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// newApiKeyFixture spins up a project with a cache-backed service, the shape
// every subtest here needs.
func newApiKeyFixture(t *testing.T, orgID string) (context.Context, *projects.Service, *projects.Repo, *testutil.TestRedis, string) {
	t.Helper()

	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	if _, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Keys Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	repo := projects.NewRepo(dbread.New(db.PgRO), rd.Client)
	svc := projects.NewService(db.PgRO, db.PgW, repo)

	proj, err := svc.CreateProject(ctx, orgID, "Keys Project", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	return ctx, svc, repo, rd, proj.ID
}

func TestCreateApiKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, svc, _, _, projectID := newApiKeyFixture(t, "org-keys-create")

	t.Run("private key is stored hashed and returned once", func(t *testing.T) {
		created, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "CI")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		if !strings.HasPrefix(created.RawKey, "prv_") || len(created.RawKey) != 24 {
			t.Errorf("RawKey = %q, want a 24-char prv_ key", created.RawKey)
		}
		if created.Key.DisplayName != "CI" {
			t.Errorf("DisplayName = %q, want %q", created.Key.DisplayName, "CI")
		}
		// The stored token is the digest — the key itself must be nowhere in the row.
		if created.Key.Token == created.RawKey {
			t.Error("private key stored in plaintext")
		}
		if created.Key.Token != sha256Hex(created.RawKey) {
			t.Errorf("Token = %q, want the key's sha256 hex", created.Key.Token)
		}
		if want := created.RawKey[:4] + "..." + created.RawKey[20:]; created.Key.Masked != want {
			t.Errorf("Masked = %q, want %q", created.Key.Masked, want)
		}
	})

	t.Run("public key is stored whole", func(t *testing.T) {
		created, err := svc.CreateApiKey(ctx, projectID, projects.KindPublic, "web")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		if !strings.HasPrefix(created.RawKey, "pub_") || len(created.RawKey) != 24 {
			t.Errorf("RawKey = %q, want a 24-char pub_ key", created.RawKey)
		}
		if created.Key.Token != created.RawKey {
			t.Errorf("Token = %q, want the key itself %q", created.Key.Token, created.RawKey)
		}
	})

	t.Run("unknown kind is refused", func(t *testing.T) {
		if _, err := svc.CreateApiKey(ctx, projectID, projects.Kind("wat"), ""); err == nil {
			t.Fatal("expected an error for an unknown kind, got nil")
		}
	})

	t.Run("a project may hold many keys of either kind", func(t *testing.T) {
		before, err := svc.ListApiKeys(ctx, projectID)
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if _, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "second"); err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		if _, err := svc.CreateApiKey(ctx, projectID, projects.KindPublic, "second"); err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		after, err := svc.ListApiKeys(ctx, projectID)
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if len(after) != len(before)+2 {
			t.Errorf("got %d keys, want %d", len(after), len(before)+2)
		}
	})
}

func TestDeleteApiKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, svc, repo, rd, projectID := newApiKeyFixture(t, "org-keys-delete")

	t.Run("a deleted key stops authenticating and leaves the cache", func(t *testing.T) {
		created, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "doomed")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		// Warm the cache — a delete that only removed the row would still
		// authenticate for as long as the entry lives.
		if _, err := repo.GetProjectByPrivateApiKey(ctx, created.RawKey); err != nil {
			t.Fatalf("resolve before delete: %v", err)
		}
		cacheKey := "project:prvkey:" + sha256Hex(created.RawKey)
		if rd.Client.Exists(ctx, cacheKey).Val() != 1 {
			t.Fatal("expected the key to be cached before deletion")
		}

		if err := svc.DeleteApiKey(ctx, projectID, created.Key.ID); err != nil {
			t.Fatalf("DeleteApiKey: %v", err)
		}

		if rd.Client.Exists(ctx, cacheKey).Val() != 0 {
			t.Error("deleted key still cached — it would keep authenticating until the TTL")
		}
		if _, err := repo.GetProjectByPrivateApiKey(ctx, created.RawKey); err == nil {
			t.Error("deleted key still resolves to its project")
		}
	})

	t.Run("the project's other keys keep working", func(t *testing.T) {
		keep, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "keep")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		drop, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "drop")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		if err := svc.DeleteApiKey(ctx, projectID, drop.Key.ID); err != nil {
			t.Fatalf("DeleteApiKey: %v", err)
		}

		if _, err := repo.GetProjectByPrivateApiKey(ctx, keep.RawKey); err != nil {
			t.Errorf("surviving key stopped resolving: %v", err)
		}
	})

	t.Run("unknown id returns ErrApiKeyNotFound", func(t *testing.T) {
		if err := svc.DeleteApiKey(ctx, projectID, "nosuchkey00000000000"); !errors.Is(err, projects.ErrApiKeyNotFound) {
			t.Fatalf("err = %v, want ErrApiKeyNotFound", err)
		}
	})

	// The delete is project-scoped in SQL: knowing another project's key id must
	// not be enough to revoke it.
	t.Run("a key of another project cannot be deleted", func(t *testing.T) {
		other, err := svc.CreateProject(ctx, "org-keys-delete", "Other Project", "")
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		victim, err := svc.CreateApiKey(ctx, other.ID, projects.KindPrivate, "victim")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}

		if err := svc.DeleteApiKey(ctx, projectID, victim.Key.ID); !errors.Is(err, projects.ErrApiKeyNotFound) {
			t.Fatalf("err = %v, want ErrApiKeyNotFound", err)
		}
		if _, err := repo.GetProjectByPrivateApiKey(ctx, victim.RawKey); err != nil {
			t.Errorf("the other project's key was revoked across the project boundary: %v", err)
		}
	})
}

// TestDeleteStarterPublicKeyRevokesIt covers the revocation that actually
// happens: a project's starter public key is, for most projects, the only key
// they ever have, so it is what an owner reaches for after leaking one. Every
// other DeleteApiKey subtest revokes a key CreateApiKey minted.
func TestDeleteStarterPublicKeyRevokesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, svc, repo, _, projectID := newApiKeyFixture(t, "org-keys-starter")

	keys, err := svc.ListApiKeys(ctx, projectID)
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Kind != string(projects.KindPublic) {
		t.Fatalf("expected a lone starter public key, got %d: %+v", len(keys), keys)
	}
	starter := keys[0]
	// A public key is stored whole, so its token is the key itself.
	pubKey := starter.Token

	if _, err := repo.GetProjectByPublicApiKey(ctx, pubKey); err != nil {
		t.Fatalf("starter key should authenticate before revocation: %v", err)
	}

	if err := svc.DeleteApiKey(ctx, projectID, starter.ID); err != nil {
		t.Fatalf("DeleteApiKey: %v", err)
	}

	// The api_keys row is the only place the key exists, so deleting it is the whole
	// revocation — nothing else resolves it.
	if _, err := repo.GetProjectByPublicApiKey(ctx, pubKey); err == nil {
		t.Error("revoked starter public key still resolves")
	}
}

// Deleting a project cascades its keys away, so their cache entries — gathered
// before the row goes — must go too, or a deleted project keeps authenticating.
func TestDeleteProjectInvalidatesItsKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, svc, repo, rd, projectID := newApiKeyFixture(t, "org-keys-cascade")

	created, err := svc.CreateApiKey(ctx, projectID, projects.KindPrivate, "doomed")
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if _, err := repo.GetProjectByPrivateApiKey(ctx, created.RawKey); err != nil {
		t.Fatalf("resolve before delete: %v", err)
	}

	if err := svc.DeleteProject(ctx, dbwrite.DeleteProjectParams{OrgID: "org-keys-cascade", ID: projectID}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	if rd.Client.Exists(ctx, "project:prvkey:"+sha256Hex(created.RawKey)).Val() != 0 {
		t.Error("a deleted project's key is still cached")
	}
	if _, err := repo.GetProjectByPrivateApiKey(ctx, created.RawKey); err == nil {
		t.Error("a deleted project's key still resolves")
	}
}
