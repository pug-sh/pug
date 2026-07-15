package devices_test

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/core/devices"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestDevicesService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	// Seed prerequisite data with raw SQL to avoid pgx/sqlc jsonb coalesce typing issues.
	for _, q := range []string{
		`INSERT INTO orgs (id, display_name) VALUES ('org-dev', 'Dev Org')`,
		`INSERT INTO projects (id, org_id, display_name) VALUES ('proj-dev', 'org-dev', 'Dev')`,
		`INSERT INTO profiles (id, project_id, properties) VALUES ('prof-1', 'proj-dev', '{}'::jsonb)`,
		// Seed two devices via raw SQL with explicit jsonb casting.
		`INSERT INTO profile_devices (id, platform, profile_id, project_id, properties, status, token) VALUES ('dev-1', 'android', 'prof-1', 'proj-dev', '{}'::jsonb, 'active', 'token-1')`,
		`INSERT INTO profile_devices (id, platform, profile_id, project_id, properties, status, token) VALUES ('dev-2', 'ios', 'prof-1', 'proj-dev', '{}'::jsonb, 'active', 'token-2')`,
	} {
		if _, err := db.PgW.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\nquery: %s", err, q)
		}
	}

	svc := devices.NewService(db.PgRO, db.PgW)

	t.Run("GetActiveDevicesByProject", func(t *testing.T) {
		list, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", "", 100)
		if err != nil {
			t.Fatalf("GetActiveDevicesByProject: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("expected 2 devices, got %d", len(list))
		}
	})

	t.Run("limit_zero_clamps_to_1000", func(t *testing.T) {
		list, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", "", 0)
		if err != nil {
			t.Fatalf("GetActiveDevicesByProject (limit=0): %v", err)
		}
		if len(list) != 2 {
			t.Errorf("expected 2 devices (clamped to 1000), got %d", len(list))
		}
	})

	t.Run("limit_negative_clamps_to_1000", func(t *testing.T) {
		list, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", "", -1)
		if err != nil {
			t.Fatalf("GetActiveDevicesByProject (limit=-1): %v", err)
		}
		if len(list) != 2 {
			t.Errorf("expected 2 devices (clamped to 1000), got %d", len(list))
		}
	})

	t.Run("respects_explicit_limit", func(t *testing.T) {
		list, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", "", 1)
		if err != nil {
			t.Fatalf("GetActiveDevicesByProject (limit=1): %v", err)
		}
		if len(list) != 1 {
			t.Errorf("expected 1 device with limit=1, got %d", len(list))
		}
	})

	t.Run("pagination_with_afterID", func(t *testing.T) {
		page1, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", "", 1)
		if err != nil {
			t.Fatalf("page1: %v", err)
		}
		if len(page1) != 1 {
			t.Fatalf("page1 expected 1, got %d", len(page1))
		}

		page2, err := svc.GetActiveDevicesByProject(ctx, "proj-dev", page1[0].ID, 1)
		if err != nil {
			t.Fatalf("page2: %v", err)
		}
		if len(page2) != 1 {
			t.Fatalf("page2 expected 1, got %d", len(page2))
		}
		if page2[0].ID == page1[0].ID {
			t.Error("page2 returned same device as page1")
		}
	})

	t.Run("UpdateDeviceStatus", func(t *testing.T) {
		dev, err := svc.UpdateDeviceStatus(ctx, "dev-1", "proj-dev", "inactive")
		if err != nil {
			t.Fatalf("UpdateDeviceStatus: %v", err)
		}
		if dev.Status != "inactive" {
			t.Errorf("Status = %q, want %q", dev.Status, "inactive")
		}
	})

	t.Run("UpdateDeviceToken", func(t *testing.T) {
		dev, err := svc.UpdateDeviceToken(ctx, "dev-1", "proj-dev", "updated-token")
		if err != nil {
			t.Fatalf("UpdateDeviceToken: %v", err)
		}
		if dev.Token.String != "updated-token" {
			t.Errorf("Token = %q, want %q", dev.Token.String, "updated-token")
		}
	})

	t.Run("empty_project_returns_empty", func(t *testing.T) {
		list, err := svc.GetActiveDevicesByProject(ctx, "proj-nonexistent", "", 100)
		if err != nil {
			t.Fatalf("GetActiveDevicesByProject (empty): %v", err)
		}
		if len(list) != 0 {
			t.Errorf("expected 0 devices for nonexistent project, got %d", len(list))
		}
	})
}
