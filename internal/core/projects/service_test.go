package projects_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
)

func TestProjectsService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	svc := projects.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	// Create a customer first — projects have a foreign key to customers.
	write := dbwrite.New(db.PgW)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           "cust-test",
		Email:        "projects@test.com",
		DisplayName:  "Test Customer",
		PasswordHash: "hash",
		PictureUri:   "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	// Hold a reference to the project created in the first subtest so later
	// subtests can use it.
	var projectID string

	t.Run("CreateProject", func(t *testing.T) {
		proj, err := svc.CreateProject(ctx, customer.ID, "My Project")
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		projectID = proj.ID

		if proj.ID == "" {
			t.Fatal("expected non-empty project ID")
		}
		if proj.DisplayName != "My Project" {
			t.Errorf("DisplayName = %q, want %q", proj.DisplayName, "My Project")
		}
		if proj.CustomerID != customer.ID {
			t.Errorf("CustomerID = %q, want %q", proj.CustomerID, customer.ID)
		}
		if !strings.HasPrefix(proj.PrivateApiKey, "prv_") {
			t.Errorf("PrivateApiKey = %q, want prefix prv_", proj.PrivateApiKey)
		}
		if !strings.HasPrefix(proj.PublicApiKey, "pub_") {
			t.Errorf("PublicApiKey = %q, want prefix pub_", proj.PublicApiKey)
		}
		if len(proj.PrivateApiKey) != 24 {
			t.Errorf("PrivateApiKey length = %d, want 24", len(proj.PrivateApiKey))
		}
		if len(proj.PublicApiKey) != 24 {
			t.Errorf("PublicApiKey length = %d, want 24", len(proj.PublicApiKey))
		}
	})

	t.Run("GetProjectByID", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		proj, err := svc.GetProjectByID(ctx, projectID)
		if err != nil {
			t.Fatalf("GetProjectByID: %v", err)
		}
		if proj.ID != projectID {
			t.Errorf("ID = %q, want %q", proj.ID, projectID)
		}
		if proj.DisplayName != "My Project" {
			t.Errorf("DisplayName = %q, want %q", proj.DisplayName, "My Project")
		}
	})

	t.Run("GetProjectsByCustomerID", func(t *testing.T) {
		// Create a second project for the same customer.
		_, err := svc.CreateProject(ctx, customer.ID, "Second Project")
		if err != nil {
			t.Fatalf("CreateProject (second): %v", err)
		}

		list, err := svc.GetProjectsByCustomerID(ctx, customer.ID)
		if err != nil {
			t.Fatalf("GetProjectsByCustomerID: %v", err)
		}
		if len(list) < 2 {
			t.Fatalf("expected at least 2 projects, got %d", len(list))
		}
	})

	t.Run("ProjectExistsForCustomer_true", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		exists, err := svc.ProjectExistsForCustomer(ctx, projectID, customer.ID)
		if err != nil {
			t.Fatalf("ProjectExistsForCustomer: %v", err)
		}
		if !exists {
			t.Error("expected true for valid project+customer, got false")
		}
	})

	t.Run("ProjectExistsForCustomer_wrong_customer", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		exists, err := svc.ProjectExistsForCustomer(ctx, projectID, "cust-nonexistent")
		if err != nil {
			t.Fatalf("ProjectExistsForCustomer: %v", err)
		}
		if exists {
			t.Error("expected false for wrong customer, got true")
		}
	})

	t.Run("UpdateProjectDisplayName", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		updated, err := svc.UpdateProjectDisplayName(ctx, dbwrite.UpdateProjectDisplayNameParams{
			ID:          projectID,
			CustomerID:  customer.ID,
			DisplayName: "Renamed Project",
		})
		if err != nil {
			t.Fatalf("UpdateProjectDisplayName: %v", err)
		}
		if updated.DisplayName != "Renamed Project" {
			t.Errorf("DisplayName = %q, want %q", updated.DisplayName, "Renamed Project")
		}

		// Confirm via read path.
		got, err := svc.GetProjectByID(ctx, projectID)
		if err != nil {
			t.Fatalf("GetProjectByID after rename: %v", err)
		}
		if got.DisplayName != "Renamed Project" {
			t.Errorf("read-path DisplayName = %q, want %q", got.DisplayName, "Renamed Project")
		}
	})

	t.Run("UpdateFCMServiceJSON", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		fcmJSON := `{"type":"service_account","project_id":"my-project"}`
		updated, err := svc.UpdateFCMServiceJSON(ctx, dbwrite.UpdateFCMServiceJSONParams{
			ID:             projectID,
			CustomerID:     customer.ID,
			FcmServiceJson: postgres.NewText(fcmJSON),
		})
		if err != nil {
			t.Fatalf("UpdateFCMServiceJSON: %v", err)
		}
		if !updated.FcmServiceJson.Valid {
			t.Fatal("expected FcmServiceJson to be valid after update")
		}
		if updated.FcmServiceJson.String != fcmJSON {
			t.Errorf("FcmServiceJson = %q, want %q", updated.FcmServiceJson.String, fcmJSON)
		}
	})

	t.Run("DeleteProject", func(t *testing.T) {
		// Create a disposable project for deletion.
		proj, err := svc.CreateProject(ctx, customer.ID, "To Delete")
		if err != nil {
			t.Fatalf("CreateProject (to delete): %v", err)
		}

		err = svc.DeleteProject(ctx, dbwrite.DeleteProjectParams{
			ID:         proj.ID,
			CustomerID: customer.ID,
		})
		if err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}

		_, err = svc.GetProjectByID(ctx, proj.ID)
		if err == nil {
			t.Fatal("expected error when getting deleted project, got nil")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("expected pgx.ErrNoRows, got: %v", err)
		}
	})
}
