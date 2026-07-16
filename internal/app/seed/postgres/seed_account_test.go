package seed_test

import (
	"context"
	"strings"
	"testing"

	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestSeedAccountApiKeys covers what the demo deployment and `pug seed` depend
// on: the seeded project comes with a working public key, and with the private
// key the seeder creates explicitly for local SDK/MCP calls. Re-running resolves
// the same project rather than seeding a second one.
func TestSeedAccountApiKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	project, err := pgseed.SeedAccount(ctx, db.PgW)
	if err != nil {
		t.Fatalf("SeedAccount: %v", err)
	}

	svc := projects.NewService(db.PgRO, db.PgW, nil)
	keys, err := svc.ListApiKeys(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2 (the starter public key and the seeded private one)", len(keys))
	}

	byKind := map[string]dbread.ApiKey{}
	for _, k := range keys {
		byKind[k.Kind] = k
	}

	pub, ok := byKind[string(projects.KindPublic)]
	if !ok {
		t.Fatal("the seeded project has no public key")
	}
	// A public key is stored whole, so the token IS the key the SDK snippet pastes.
	if !strings.HasPrefix(pub.Token, "pub_") {
		t.Errorf("public key token = %q, want a pub_ key", pub.Token)
	}

	// The private key is only reachable as a digest here; that it exists at all is
	// what `pug worker demo` and local SDK calls need.
	if _, ok := byKind[string(projects.KindPrivate)]; !ok {
		t.Error("the seeded project has no private key — local dev has nothing to call the SDK/MCP endpoints with")
	}

	t.Run("re-seeding resolves the same project", func(t *testing.T) {
		again, err := pgseed.SeedAccount(ctx, db.PgW)
		if err != nil {
			t.Fatalf("SeedAccount (second run): %v", err)
		}
		if again.ID != project.ID {
			t.Errorf("second run returned project %q, want the first run's %q", again.ID, project.ID)
		}

		keys, err := svc.ListApiKeys(ctx, project.ID)
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if len(keys) != 2 {
			t.Errorf("got %d keys after re-seeding, want the original 2", len(keys))
		}
	})
}
