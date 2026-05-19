package projects_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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
		proj, err := svc.CreateProject(ctx, org.ID, "My Project")
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
		if _, err := svc.CreateProject(ctx, org.ID, "Second Project"); err != nil {
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

	t.Run("UpdateProjectDisplayName", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		updated, err := svc.UpdateProjectDisplayName(ctx, dbwrite.UpdateProjectDisplayNameParams{
			ID:          projectID,
			OrgID:       org.ID,
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
		proj, err := svc.CreateProject(ctx, org.ID, "To Delete")
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

	t.Run("DashboardCRUD", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Overview", "Executive metrics")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}

		insightQuery := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
				To:   timestamppb.New(time.Now()),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
			},
		}

		createdInsight, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Signups", "Tracks signup volume",
			projects.InsightTile{Query: insightQuery},
			[]*dashboardsv1.ResponsiveGridLayout{
				{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			},
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile insight: %v", err)
		}
		if createdInsight.Kind != int16(projects.TileKindInsight) {
			t.Fatalf("createdInsight.Kind = %d, want %d", createdInsight.Kind, projects.TileKindInsight)
		}
		if createdInsight.MarkdownBody.Valid {
			t.Fatalf("createdInsight.MarkdownBody.Valid = true, want false")
		}

		markdownBody := "# Note\n\nSee chart above. ![logo](https://example.com/logo.png)"
		createdMarkdown, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Context", "",
			projects.MarkdownTile{Body: markdownBody},
			[]*dashboardsv1.ResponsiveGridLayout{
				{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(4), W: proto.Int32(12), H: proto.Int32(3)},
			},
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile markdown: %v", err)
		}
		if createdMarkdown.Kind != int16(projects.TileKindMarkdown) {
			t.Fatalf("createdMarkdown.Kind = %d, want %d", createdMarkdown.Kind, projects.TileKindMarkdown)
		}
		if createdMarkdown.InsightQuery != nil {
			t.Fatalf("createdMarkdown.InsightQuery = %v, want nil", createdMarkdown.InsightQuery)
		}
		if !createdMarkdown.MarkdownBody.Valid || createdMarkdown.MarkdownBody.String != markdownBody {
			t.Fatalf("createdMarkdown.MarkdownBody = %+v, want %q", createdMarkdown.MarkdownBody, markdownBody)
		}

		gotDashboard, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		if gotDashboard.Dashboard.DisplayName != "Overview" {
			t.Fatalf("DisplayName = %q, want %q", gotDashboard.Dashboard.DisplayName, "Overview")
		}
		if len(gotDashboard.Tiles) != 2 {
			t.Fatalf("tiles = %d, want 2", len(gotDashboard.Tiles))
		}
		// Look up tiles by ID rather than position — the read-side SQL orders by
		// create_time asc without a tiebreaker, so two near-simultaneous inserts
		// could plausibly invert.
		insightTile := tileByID(t, gotDashboard.Tiles, createdInsight.ID)
		markdownTile := tileByID(t, gotDashboard.Tiles, createdMarkdown.ID)

		if insightTile.Kind != int16(projects.TileKindInsight) {
			t.Fatalf("insightTile.Kind = %d, want INSIGHT", insightTile.Kind)
		}
		if insightTile.InsightQuery["insightType"] != "INSIGHT_TYPE_TRENDS" {
			t.Fatalf("insightTile insightType = %v, want INSIGHT_TYPE_TRENDS", insightTile.InsightQuery["insightType"])
		}
		if markdownTile.Kind != int16(projects.TileKindMarkdown) {
			t.Fatalf("markdownTile.Kind = %d, want MARKDOWN", markdownTile.Kind)
		}
		if markdownTile.MarkdownBody.String != markdownBody {
			t.Fatalf("markdownTile body = %q, want %q", markdownTile.MarkdownBody.String, markdownBody)
		}

		layout, ok := insightTile.Layouts["lg"].(map[string]any)
		if !ok {
			t.Fatalf("expected lg layout map, got %T", insightTile.Layouts["lg"])
		}
		if layout["w"] != float64(6) {
			t.Fatalf("Insight layout width = %v, want %v", layout["w"], float64(6))
		}

		updatedInsight, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, createdInsight.ID, "Activated Users", "Tracks activation volume",
			projects.InsightTile{Query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Now().Add(-7 * 24 * time.Hour)),
					To:   timestamppb.New(time.Now()),
				},
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("activated")}},
				},
			}},
			[]*dashboardsv1.ResponsiveGridLayout{
				{Breakpoint: proto.String("lg"), X: proto.Int32(6), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(5)},
				{Breakpoint: proto.String("md"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(10), H: proto.Int32(6)},
			},
		)
		if err != nil {
			t.Fatalf("UpdateDashboardTile: %v", err)
		}
		if updatedInsight.DisplayName != "Activated Users" {
			t.Fatalf("DisplayName = %q, want %q", updatedInsight.DisplayName, "Activated Users")
		}

		list, err := svc.ListDashboards(ctx, projectID)
		if err != nil {
			t.Fatalf("ListDashboards: %v", err)
		}
		if len(list) == 0 {
			t.Fatal("expected at least one dashboard")
		}
		if len(list[0].Tiles) != 2 {
			t.Fatalf("list tiles = %d, want 2", len(list[0].Tiles))
		}

		if err := svc.DeleteDashboardTile(ctx, projectID, dashboard.ID, createdInsight.ID); err != nil {
			t.Fatalf("DeleteDashboardTile insight: %v", err)
		}
		if err := svc.DeleteDashboardTile(ctx, projectID, dashboard.ID, createdMarkdown.ID); err != nil {
			t.Fatalf("DeleteDashboardTile markdown: %v", err)
		}

		if err := svc.DeleteDashboard(ctx, projectID, dashboard.ID); err != nil {
			t.Fatalf("DeleteDashboard: %v", err)
		}
	})

	t.Run("DashboardTileKindSwap", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Swap Dashboard", "")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() {
			_ = svc.DeleteDashboard(ctx, projectID, dashboard.ID)
		})

		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Initially Insight", "",
			projects.InsightTile{Query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
					To:   timestamppb.New(time.Now()),
				},
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
				},
			}},
			nil,
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile (insight): %v", err)
		}

		body := "Now I'm markdown"
		swapped, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, tile.ID, "Now Markdown", "",
			projects.MarkdownTile{Body: body},
			nil,
		)
		if err != nil {
			t.Fatalf("UpdateDashboardTile (swap to markdown): %v", err)
		}
		if swapped.Kind != int16(projects.TileKindMarkdown) {
			t.Fatalf("swapped.Kind = %d, want %d", swapped.Kind, projects.TileKindMarkdown)
		}
		if swapped.InsightQuery != nil {
			t.Fatalf("swapped.InsightQuery = %v, want nil (the CHECK constraint should have rejected non-nil)", swapped.InsightQuery)
		}
		if !swapped.MarkdownBody.Valid || swapped.MarkdownBody.String != body {
			t.Fatalf("swapped.MarkdownBody = %+v, want %q", swapped.MarkdownBody, body)
		}

		// Round-trip through GetDashboard to confirm the read side also sees the swap.
		got, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		if len(got.Tiles) != 1 {
			t.Fatalf("got.Tiles = %d, want 1", len(got.Tiles))
		}
		if got.Tiles[0].Kind != int16(projects.TileKindMarkdown) {
			t.Fatalf("read-side Tiles[0].Kind = %d, want %d", got.Tiles[0].Kind, projects.TileKindMarkdown)
		}
		if got.Tiles[0].InsightQuery != nil {
			t.Fatalf("read-side Tiles[0].InsightQuery = %v, want nil", got.Tiles[0].InsightQuery)
		}
	})

	t.Run("DashboardNotFoundPaths", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		const missingID = "missing0000000000000"

		if _, err := svc.GetDashboard(ctx, projectID, missingID); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("GetDashboard not-found err = %v, want ErrDashboardNotFound", err)
		}
		if _, err := svc.UpdateDashboardDisplayName(ctx, projectID, missingID, "x", ""); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("UpdateDashboardDisplayName not-found err = %v, want ErrDashboardNotFound", err)
		}
		if err := svc.DeleteDashboard(ctx, projectID, missingID); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("DeleteDashboard not-found err = %v, want ErrDashboardNotFound", err)
		}
	})

	t.Run("DashboardRenamePreservesTiles", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		// Pre-I1, UpdateDashboardDisplayName returned a Dashboard with nil tiles,
		// so the RPC response silently dropped them. This pins both the
		// happy-path rename and the tile preservation in the returned shape.
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Original", "first desc")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboard.ID) })

		body := "tile body"
		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Tile", "",
			projects.MarkdownTile{Body: body}, nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile: %v", err)
		}

		// Empty description must be preserved (partial-update semantics from I3).
		renamed, err := svc.UpdateDashboardDisplayName(ctx, projectID, dashboard.ID, "Renamed", "")
		if err != nil {
			t.Fatalf("UpdateDashboardDisplayName: %v", err)
		}
		if renamed.Dashboard.DisplayName != "Renamed" {
			t.Errorf("DisplayName = %q, want %q", renamed.Dashboard.DisplayName, "Renamed")
		}
		if renamed.Dashboard.Description != "first desc" {
			t.Errorf("Description = %q, want %q (empty input should preserve)", renamed.Dashboard.Description, "first desc")
		}
		if len(renamed.Tiles) != 1 || renamed.Tiles[0].ID != tile.ID {
			t.Fatalf("Tiles = %+v, want single tile %s", renamed.Tiles, tile.ID)
		}

		// Non-empty description must overwrite.
		renamed2, err := svc.UpdateDashboardDisplayName(ctx, projectID, dashboard.ID, "Renamed2", "new desc")
		if err != nil {
			t.Fatalf("UpdateDashboardDisplayName second: %v", err)
		}
		if renamed2.Dashboard.Description != "new desc" {
			t.Errorf("Description = %q, want %q (non-empty input should overwrite)", renamed2.Dashboard.Description, "new desc")
		}
	})

	t.Run("DashboardCrossProjectIsolation", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		// Create a second project in the same org. Operations from project B against
		// project A's dashboard/tile must hit the join filter and return the
		// not-found sentinel — a "simplification" that drops the join in any of
		// the write queries would silently let project B touch project A's data.
		projB, err := svc.CreateProject(ctx, org.ID, "Isolation B")
		if err != nil {
			t.Fatalf("CreateProject B: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteProject(ctx, dbwrite.DeleteProjectParams{ID: projB.ID, OrgID: org.ID}) })

		dashboardA, err := svc.CreateDashboard(ctx, projectID, "Isolation A", "")
		if err != nil {
			t.Fatalf("CreateDashboard A: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboardA.ID) })

		body := "tile in A"
		tileA, err := svc.CreateDashboardTile(ctx, projectID, dashboardA.ID, "Tile A", "",
			projects.MarkdownTile{Body: body}, nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile A: %v", err)
		}

		// Reads from project B must not see project A's dashboard.
		if _, err := svc.GetDashboard(ctx, projB.ID, dashboardA.ID); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("GetDashboard cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// Writes against the dashboard from project B must not succeed.
		if _, err := svc.UpdateDashboardDisplayName(ctx, projB.ID, dashboardA.ID, "hijack", ""); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("UpdateDashboardDisplayName cross-project err = %v, want ErrDashboardNotFound", err)
		}
		if err := svc.DeleteDashboard(ctx, projB.ID, dashboardA.ID); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("DeleteDashboard cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// CreateDashboardTile against project A's dashboard from project B must
		// fail with ErrDashboardNotFound — the insert's WHERE clause depends on
		// the project_id filter joining dashboards.
		body2 := "hijack body"
		if _, err := svc.CreateDashboardTile(ctx, projB.ID, dashboardA.ID, "Hijack", "",
			projects.MarkdownTile{Body: body2}, nil); !errors.Is(err, projects.ErrDashboardNotFound) {
			t.Errorf("CreateDashboardTile cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// Tile-level writes from project B against project A's tile must surface
		// ErrDashboardTileNotFound (the join on dashboards.project_id eliminates
		// the row in the update's FROM clause).
		hijackBody := "hijack rename"
		if _, err := svc.UpdateDashboardTile(ctx, projB.ID, dashboardA.ID, tileA.ID, "hijack", "",
			projects.MarkdownTile{Body: hijackBody}, nil); !errors.Is(err, projects.ErrDashboardTileNotFound) {
			t.Errorf("UpdateDashboardTile cross-project err = %v, want ErrDashboardTileNotFound", err)
		}
		if err := svc.DeleteDashboardTile(ctx, projB.ID, dashboardA.ID, tileA.ID); !errors.Is(err, projects.ErrDashboardTileNotFound) {
			t.Errorf("DeleteDashboardTile cross-project err = %v, want ErrDashboardTileNotFound", err)
		}

		// Confirm the tile is still intact in project A — none of the cross-project
		// attempts above mutated it.
		got, err := svc.GetDashboard(ctx, projectID, dashboardA.ID)
		if err != nil {
			t.Fatalf("GetDashboard A after cross-project attempts: %v", err)
		}
		if len(got.Tiles) != 1 || got.Tiles[0].ID != tileA.ID || got.Tiles[0].DisplayName != "Tile A" {
			t.Errorf("tile mutated by cross-project attempts: %+v", got.Tiles)
		}
	})

	t.Run("DashboardTileDisplayNameConflict", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Conflict Dashboard", "")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() {
			_ = svc.DeleteDashboard(ctx, projectID, dashboard.ID)
		})

		bodyA := "a"
		bodyB := "b"
		bodyC := "c"
		bodyD := "d"

		// Two titled tiles with the same display name (case-insensitive) — second should conflict.
		if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Notes", "",
			projects.MarkdownTile{Body: bodyA}, nil); err != nil {
			t.Fatalf("first titled tile: %v", err)
		}
		_, err = svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "notes", "",
			projects.MarkdownTile{Body: bodyB}, nil)
		if !errors.Is(err, projects.ErrDashboardTileDisplayNameConflict) {
			t.Fatalf("second titled tile err = %v, want ErrDashboardTileDisplayNameConflict", err)
		}

		// Two untitled tiles in the same dashboard — both should succeed (partial-index exemption).
		if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "", "",
			projects.MarkdownTile{Body: bodyC}, nil); err != nil {
			t.Fatalf("first untitled tile: %v", err)
		}
		if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "", "",
			projects.MarkdownTile{Body: bodyD}, nil); err != nil {
			t.Fatalf("second untitled tile: %v", err)
		}

		// Update-into-conflict: take the second untitled tile, rename it to "Notes" → conflict.
		list, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		var untitledID string
		for _, ti := range list.Tiles {
			if ti.DisplayName == "" && ti.MarkdownBody.String == bodyD {
				untitledID = ti.ID
				break
			}
		}
		if untitledID == "" {
			t.Fatal("could not find the second untitled tile")
		}
		_, err = svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, untitledID, "Notes", "",
			projects.MarkdownTile{Body: bodyD}, nil)
		if !errors.Is(err, projects.ErrDashboardTileDisplayNameConflict) {
			t.Fatalf("rename-into-conflict err = %v, want ErrDashboardTileDisplayNameConflict", err)
		}
	})

	t.Run("DashboardTilePartialUpdate", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		// The write query uses coalesce(nullif(@field, ''), dt.field) on display_name
		// and description: an empty string in the request preserves the prior value.
		// A SQL "simplification" that drops the coalesce would silently let clients
		// blank out tile names — this subtest pins the cross-layer contract.
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Partial Update Dashboard", "")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboard.ID) })

		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Original Title", "original desc",
			projects.MarkdownTile{Body: "body"}, nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile: %v", err)
		}

		// Update with empty DisplayName and Description — both must be preserved.
		preserved, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, tile.ID, "", "",
			projects.MarkdownTile{Body: "body"}, nil)
		if err != nil {
			t.Fatalf("UpdateDashboardTile (preserve): %v", err)
		}
		if preserved.DisplayName != "Original Title" {
			t.Errorf("DisplayName = %q, want %q (empty input should preserve)", preserved.DisplayName, "Original Title")
		}
		if preserved.Description != "original desc" {
			t.Errorf("Description = %q, want %q (empty input should preserve)", preserved.Description, "original desc")
		}

		// Update with non-empty values — both must be overwritten.
		updated, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, tile.ID, "New Title", "new desc",
			projects.MarkdownTile{Body: "body"}, nil)
		if err != nil {
			t.Fatalf("UpdateDashboardTile (overwrite): %v", err)
		}
		if updated.DisplayName != "New Title" {
			t.Errorf("DisplayName = %q, want %q (non-empty input should overwrite)", updated.DisplayName, "New Title")
		}
		if updated.Description != "new desc" {
			t.Errorf("Description = %q, want %q (non-empty input should overwrite)", updated.Description, "new desc")
		}
	})

	t.Run("EmptyDashboardRead", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		// Pin the empty-tiles read path: a dashboard with no tiles must produce a
		// well-formed DashboardWithTiles with an empty/nil Tiles slice. Existing
		// subtests always create at least one tile before reading.
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Empty Dashboard", "")
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboard.ID) })

		got, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		if len(got.Tiles) != 0 {
			t.Errorf("Tiles = %d, want 0", len(got.Tiles))
		}
		if got.Dashboard.ID != dashboard.ID {
			t.Errorf("Dashboard.ID = %q, want %q", got.Dashboard.ID, dashboard.ID)
		}
	})
}

func tileByID(t *testing.T, tiles []dbread.DashboardTile, id string) dbread.DashboardTile {
	t.Helper()
	for _, tile := range tiles {
		if tile.ID == id {
			return tile
		}
	}
	t.Fatalf("no tile with id %q in %d-tile slice", id, len(tiles))
	return dbread.DashboardTile{}
}
