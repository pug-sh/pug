package profiles_test

import (
	"context"
	"testing"

	"github.com/rs/xid"

	"github.com/fivebitsio/cotton/internal/core/profiles"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/testutil"
)

func TestRepoGetPropertyKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedTestProject(t, ctx, pg)
	seedProfiles(t, ctx, pg, projectID)

	repo := profiles.NewRepo(dbread.New(pg.PgRO))

	keys, err := repo.GetPropertyKeys(ctx, projectID)
	if err != nil {
		t.Fatalf("GetPropertyKeys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one property key")
	}

	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	if !keySet["plan"] || !keySet["role"] {
		t.Errorf("expected plan and role in keys, got: %v", keySet)
	}
}

func TestRepoGetPropertyValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedTestProject(t, ctx, pg)
	seedProfiles(t, ctx, pg, projectID)

	repo := profiles.NewRepo(dbread.New(pg.PgRO))

	t.Run("existing_key", func(t *testing.T) {
		values, err := repo.GetPropertyValues(ctx, projectID, "plan")
		if err != nil {
			t.Fatalf("GetPropertyValues: %v", err)
		}
		if len(values) != 2 {
			t.Fatalf("expected 2 values (free, pro), got %d: %v", len(values), values)
		}
		valSet := map[string]bool{}
		for _, v := range values {
			valSet[v] = true
		}
		if !valSet["pro"] || !valSet["free"] {
			t.Errorf("expected pro and free, got: %v", valSet)
		}
	})

	t.Run("nonexistent_key", func(t *testing.T) {
		values, err := repo.GetPropertyValues(ctx, projectID, "nonexistent")
		if err != nil {
			t.Fatalf("GetPropertyValues: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("expected 0 values for nonexistent key, got %d: %v", len(values), values)
		}
	})

	t.Run("empty_project", func(t *testing.T) {
		emptyProjectID := seedTestProject(t, ctx, pg)
		values, err := repo.GetPropertyValues(ctx, emptyProjectID, "plan")
		if err != nil {
			t.Fatalf("GetPropertyValues: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("expected 0 values for empty project, got %d: %v", len(values), values)
		}
	})
}

func TestNewRepoPanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil queries")
		}
	}()
	profiles.NewRepo(nil)
}

func seedTestProject(t *testing.T, ctx context.Context, pg *testutil.TestPostgres) string {
	t.Helper()

	orgID := xid.New().String()
	projectID := xid.New().String()

	_, err := pg.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}

	_, err = pg.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project",
		xid.New().String()+"test", // 24 chars
		xid.New().String()+"test", // 24 chars
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	return projectID
}

func seedProfiles(t *testing.T, ctx context.Context, pg *testutil.TestPostgres, projectID string) {
	t.Helper()

	profs := []struct {
		externalID string
		properties string
	}{
		{"alice", `{"plan": "pro", "role": "admin"}`},
		{"bob", `{"plan": "free", "role": "member"}`},
	}

	for _, p := range profs {
		_, err := pg.PgW.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties) VALUES ($1, $2, $3, $4::jsonb)`,
			xid.New().String(),
			projectID,
			p.externalID,
			p.properties,
		)
		if err != nil {
			t.Fatalf("insert profile: %v", err)
		}
	}
}
