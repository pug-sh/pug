package projects_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestProjectsService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	svc := projects.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	// Create a customer and org — projects belong to orgs, and membership
	// checks require a customer in org_members.
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

	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-test",
		DisplayName: "Test Org",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if _, err = write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	// Hold a reference to the project created in the first subtest so later
	// subtests can use it.
	var projectID string

	t.Run("CreateProject", func(t *testing.T) {
		proj, err := svc.CreateProject(ctx, org.ID, "My Project", "")
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
		if proj.OrgID != org.ID {
			t.Errorf("OrgID = %q, want %q", proj.OrgID, org.ID)
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

	t.Run("GetProjectsByOrgID", func(t *testing.T) {
		// Create a second project for the same org.
		if _, err := svc.CreateProject(ctx, org.ID, "Second Project", ""); err != nil {
			t.Fatalf("CreateProject (second): %v", err)
		}

		list, err := svc.GetProjectsByOrgID(ctx, org.ID)
		if err != nil {
			t.Fatalf("GetProjectsByOrgID: %v", err)
		}
		if len(list) < 2 {
			t.Fatalf("expected at least 2 projects, got %d", len(list))
		}
	})

	t.Run("ProjectExistsForOrgMember_true", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		exists, err := svc.ProjectExistsForOrgMember(ctx, projectID, customer.ID)
		if err != nil {
			t.Fatalf("ProjectExistsForOrgMember: %v", err)
		}
		if !exists {
			t.Error("expected true for valid project+org member, got false")
		}
	})

	t.Run("ProjectExistsForOrgMember_wrong_customer", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		exists, err := svc.ProjectExistsForOrgMember(ctx, projectID, "cust-nonexistent")
		if err != nil {
			t.Fatalf("ProjectExistsForOrgMember: %v", err)
		}
		if exists {
			t.Error("expected false for wrong customer, got true")
		}
	})

	t.Run("UpdateProjectMeta", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		updated, err := svc.UpdateProjectMeta(ctx, dbwrite.UpdateProjectMetaParams{
			ID:                projectID,
			OrgID:             org.ID,
			DisplayName:       postgres.NewText("Renamed Project"),
			ReportingTimezone: postgres.NewText("Asia/Kolkata"),
		})
		if err != nil {
			t.Fatalf("UpdateProjectMeta: %v", err)
		}
		if updated.DisplayName != "Renamed Project" {
			t.Errorf("DisplayName = %q, want %q", updated.DisplayName, "Renamed Project")
		}
		if updated.ReportingTimezone != "Asia/Kolkata" {
			t.Errorf("ReportingTimezone = %q, want %q", updated.ReportingTimezone, "Asia/Kolkata")
		}

		// Confirm via read path.
		got, err := svc.GetProjectByID(ctx, projectID)
		if err != nil {
			t.Fatalf("GetProjectByID after update: %v", err)
		}
		if got.DisplayName != "Renamed Project" {
			t.Errorf("read-path DisplayName = %q, want %q", got.DisplayName, "Renamed Project")
		}
		if got.ReportingTimezone != "Asia/Kolkata" {
			t.Errorf("read-path ReportingTimezone = %q, want %q", got.ReportingTimezone, "Asia/Kolkata")
		}
	})

	t.Run("UpdateFCMServiceJSON", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		fcmJSON := `{"type":"service_account","project_id":"my-project"}`
		updated, err := svc.UpdateFCMServiceJSON(ctx, dbwrite.UpdateFCMServiceJSONParams{
			ID:             projectID,
			OrgID:          org.ID,
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
		proj, err := svc.CreateProject(ctx, org.ID, "To Delete", "")
		if err != nil {
			t.Fatalf("CreateProject (to delete): %v", err)
		}

		err = svc.DeleteProject(ctx, dbwrite.DeleteProjectParams{
			ID:    proj.ID,
			OrgID: org.ID,
		})
		if err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}

		if _, err = svc.GetProjectByID(ctx, proj.ID); err == nil {
			t.Fatal("expected error when getting deleted project, got nil")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("expected pgx.ErrNoRows, got: %v", err)
		}
	})
}

// TestCreateProjectAsAdmin pins the race-safe admin gate that lives in the
// CreateProjectAsAdmin SQL CTE — the authoritative check behind projects.Create
// (the AuthzInterceptor is the coarse pre-check; this CTE is the atomic one). Only
// an org ADMIN may create a project: a member, a viewer, and a non-member each get
// ErrAdminRequired (no row inserted), and the admin succeeds.
func TestCreateProjectAsAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	svc := projects.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()
	write := dbwrite.New(db.PgW)

	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-cpa", DisplayName: "CPA Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// seed creates a customer and, when role != "", an org_members row carrying it.
	seed := func(id, role string) string {
		if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
			ID: id, Email: id + "@cpa.test", DisplayName: id, PasswordHash: "h", PictureUri: "",
		}); err != nil {
			t.Fatalf("CreateCustomer %s: %v", id, err)
		}
		if role != "" {
			if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
				OrgID: org.ID, CustomerID: id, Role: role,
			}); err != nil {
				t.Fatalf("CreateOrgMember %s: %v", id, err)
			}
		}
		return id
	}

	admin := seed("cpa-admin", "ORG_ROLE_ADMIN")
	member := seed("cpa-member", "ORG_ROLE_MEMBER")
	viewer := seed("cpa-viewer", "ORG_ROLE_VIEWER")
	nonMember := seed("cpa-outsider", "")

	for _, tc := range []struct{ name, customerID string }{
		{"member", member},
		{"viewer", viewer},
		{"non-member", nonMember},
	} {
		t.Run(tc.name+" is denied", func(t *testing.T) {
			if _, err := svc.CreateProjectAsAdmin(ctx, org.ID, tc.customerID, "p-"+tc.name, ""); !errors.Is(err, projects.ErrAdminRequired) {
				t.Fatalf("want ErrAdminRequired, got %v", err)
			}
			// The CTE must skip the INSERT on a failed admin check. The denial
			// cases all run before the admin succeeds, so the org stays empty —
			// confirm no row leaked through.
			if projs, err := svc.GetProjectsByOrgID(ctx, org.ID); err != nil {
				t.Fatalf("GetProjectsByOrgID: %v", err)
			} else if len(projs) != 0 {
				t.Fatalf("expected no projects after denied create, got %d", len(projs))
			}
		})
	}

	t.Run("admin is allowed", func(t *testing.T) {
		proj, err := svc.CreateProjectAsAdmin(ctx, org.ID, admin, "admin project", "")
		if err != nil {
			t.Fatalf("admin CreateProjectAsAdmin: %v", err)
		}
		if proj.ID == "" || proj.OrgID != org.ID {
			t.Fatalf("unexpected project %+v", proj)
		}
	})
}
