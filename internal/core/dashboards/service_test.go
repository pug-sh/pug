package dashboards_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pug-sh/pug/internal/core/dashboards"
	"github.com/pug-sh/pug/internal/core/projects"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

func TestDashboardsService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	projectsSvc := projects.NewService(db.PgRO, db.PgW, nil)
	svc := dashboards.NewService(db.PgRO, db.PgW)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           "cust-dashboard-test",
		Email:        "dashboards@test.com",
		DisplayName:  "Test Customer",
		PasswordHash: "hash",
		PictureUri:   "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-dashboard-test",
		DisplayName: "Test Org",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	project, err := projectsSvc.CreateProject(ctx, org.ID, "Dashboard Test Project")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projectID := project.ID

	// insightSpec builds a minimal valid insight tile payload (what to measure;
	// the window lives on the dashboard).
	insightSpec := func(kind string) *insightsv1.InsightQuerySpec {
		return &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String(kind)}},
			},
		}
	}

	t.Run("DashboardCRUD", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Overview", "Executive metrics",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
			insightsv1.Granularity_GRANULARITY_DAY)
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		if dashboard.DefaultTimeRange != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.String() {
			t.Fatalf("dashboard.DefaultTimeRange = %q, want LAST_7_DAYS", dashboard.DefaultTimeRange)
		}
		if dashboard.DefaultGranularity != insightsv1.Granularity_GRANULARITY_DAY.String() {
			t.Fatalf("dashboard.DefaultGranularity = %q, want GRANULARITY_DAY", dashboard.DefaultGranularity)
		}

		createdInsight, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Signups", "Tracks signup volume",
			dashboards.InsightTile{Spec: insightSpec("signup")},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			[]*dashboardsv1.ResponsiveGridLayout{
				{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			},
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile insight: %v", err)
		}
		if createdInsight.Kind != int16(dashboards.TileKindInsight) {
			t.Fatalf("createdInsight.Kind = %d, want %d", createdInsight.Kind, dashboards.TileKindInsight)
		}
		if createdInsight.MarkdownBody.Valid {
			t.Fatalf("createdInsight.MarkdownBody.Valid = true, want false")
		}
		if createdInsight.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.String() {
			t.Fatalf("createdInsight.ViewMode = %q, want LINE", createdInsight.ViewMode)
		}

		markdownBody := "# Note\n\nSee chart above. ![logo](https://example.com/logo.png)"
		createdMarkdown, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Context", "",
			dashboards.MarkdownTile{Body: markdownBody},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			[]*dashboardsv1.ResponsiveGridLayout{
				{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(4), W: proto.Int32(12), H: proto.Int32(3)},
			},
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile markdown: %v", err)
		}
		if createdMarkdown.Kind != int16(dashboards.TileKindMarkdown) {
			t.Fatalf("createdMarkdown.Kind = %d, want %d", createdMarkdown.Kind, dashboards.TileKindMarkdown)
		}
		if createdMarkdown.InsightQuery != nil {
			t.Fatalf("createdMarkdown.InsightQuery = %v, want nil", createdMarkdown.InsightQuery)
		}
		if !createdMarkdown.MarkdownBody.Valid || createdMarkdown.MarkdownBody.String != markdownBody {
			t.Fatalf("createdMarkdown.MarkdownBody = %+v, want %q", createdMarkdown.MarkdownBody, markdownBody)
		}
		if createdMarkdown.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String() {
			t.Fatalf("createdMarkdown.ViewMode = %q, want UNSPECIFIED", createdMarkdown.ViewMode)
		}

		gotDashboard, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		if gotDashboard.Dashboard.DisplayName != "Overview" {
			t.Fatalf("DisplayName = %q, want %q", gotDashboard.Dashboard.DisplayName, "Overview")
		}
		if gotDashboard.Dashboard.DefaultTimeRange != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.String() {
			t.Fatalf("read-side DefaultTimeRange = %q, want LAST_7_DAYS", gotDashboard.Dashboard.DefaultTimeRange)
		}
		if len(gotDashboard.Tiles) != 2 {
			t.Fatalf("tiles = %d, want 2", len(gotDashboard.Tiles))
		}
		// Look up tiles by ID rather than position — the read-side SQL orders by
		// create_time asc without a tiebreaker, so two near-simultaneous inserts
		// could plausibly invert.
		insightTile := tileByID(t, gotDashboard.Tiles, createdInsight.ID)
		markdownTile := tileByID(t, gotDashboard.Tiles, createdMarkdown.ID)

		if insightTile.Kind != int16(dashboards.TileKindInsight) {
			t.Fatalf("insightTile.Kind = %d, want INSIGHT", insightTile.Kind)
		}
		// The tile stores an InsightQuerySpec, so insightType is top-level and there
		// is no granularity/time_range on the tile.
		if insightTile.InsightQuery["insightType"] != "INSIGHT_TYPE_TRENDS" {
			t.Fatalf("insightTile insightType = %v, want INSIGHT_TYPE_TRENDS", insightTile.InsightQuery["insightType"])
		}
		if _, ok := insightTile.InsightQuery["granularity"]; ok {
			t.Fatalf("insightTile must not store granularity (it is dashboard-level)")
		}
		if insightTile.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.String() {
			t.Fatalf("insightTile.ViewMode = %q, want LINE", insightTile.ViewMode)
		}
		if markdownTile.Kind != int16(dashboards.TileKindMarkdown) {
			t.Fatalf("markdownTile.Kind = %d, want MARKDOWN", markdownTile.Kind)
		}
		if markdownTile.MarkdownBody.String != markdownBody {
			t.Fatalf("markdownTile body = %q, want %q", markdownTile.MarkdownBody.String, markdownBody)
		}
		if markdownTile.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String() {
			t.Fatalf("markdownTile.ViewMode = %q, want UNSPECIFIED", markdownTile.ViewMode)
		}

		layout, ok := insightTile.Layouts["lg"].(map[string]any)
		if !ok {
			t.Fatalf("expected lg layout map, got %T", insightTile.Layouts["lg"])
		}
		if layout["w"] != float64(6) {
			t.Fatalf("Insight layout width = %v, want %v", layout["w"], float64(6))
		}

		updatedInsight, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, createdInsight.ID, "Activated Users", "Tracks activation volume",
			dashboards.InsightTile{Spec: insightSpec("activated")},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA,
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
		if updatedInsight.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.String() {
			t.Fatalf("updatedInsight.ViewMode = %q, want AREA", updatedInsight.ViewMode)
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

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Swap Dashboard", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED)
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() {
			_ = svc.DeleteDashboard(ctx, projectID, dashboard.ID)
		})

		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Initially Insight", "",
			dashboards.InsightTile{Spec: insightSpec("signup")},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			nil,
		)
		if err != nil {
			t.Fatalf("CreateDashboardTile (insight): %v", err)
		}

		body := "Now I'm markdown"
		swapped, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, tile.ID, "Now Markdown", "",
			dashboards.MarkdownTile{Body: body},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			nil,
		)
		if err != nil {
			t.Fatalf("UpdateDashboardTile (swap to markdown): %v", err)
		}
		if swapped.Kind != int16(dashboards.TileKindMarkdown) {
			t.Fatalf("swapped.Kind = %d, want %d", swapped.Kind, dashboards.TileKindMarkdown)
		}
		if swapped.InsightQuery != nil {
			t.Fatalf("swapped.InsightQuery = %v, want nil (the CHECK constraint should have rejected non-nil)", swapped.InsightQuery)
		}
		if !swapped.MarkdownBody.Valid || swapped.MarkdownBody.String != body {
			t.Fatalf("swapped.MarkdownBody = %+v, want %q", swapped.MarkdownBody, body)
		}
		if swapped.ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String() {
			t.Fatalf("swapped.ViewMode = %q, want UNSPECIFIED", swapped.ViewMode)
		}

		// Round-trip through GetDashboard to confirm the read side also sees the swap.
		got, err := svc.GetDashboard(ctx, projectID, dashboard.ID)
		if err != nil {
			t.Fatalf("GetDashboard: %v", err)
		}
		if len(got.Tiles) != 1 {
			t.Fatalf("got.Tiles = %d, want 1", len(got.Tiles))
		}
		if got.Tiles[0].Kind != int16(dashboards.TileKindMarkdown) {
			t.Fatalf("read-side Tiles[0].Kind = %d, want %d", got.Tiles[0].Kind, dashboards.TileKindMarkdown)
		}
		if got.Tiles[0].InsightQuery != nil {
			t.Fatalf("read-side Tiles[0].InsightQuery = %v, want nil", got.Tiles[0].InsightQuery)
		}
		if got.Tiles[0].ViewMode != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String() {
			t.Fatalf("read-side Tiles[0].ViewMode = %q, want UNSPECIFIED", got.Tiles[0].ViewMode)
		}
	})

	t.Run("DashboardNotFoundPaths", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}
		const missingID = "missing0000000000000"

		if _, err := svc.GetDashboard(ctx, projectID, missingID); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("GetDashboard not-found err = %v, want ErrDashboardNotFound", err)
		}
		if _, err := svc.UpdateDashboard(ctx, projectID, missingID, "x", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("UpdateDashboard not-found err = %v, want ErrDashboardNotFound", err)
		}
		if err := svc.DeleteDashboard(ctx, projectID, missingID); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("DeleteDashboard not-found err = %v, want ErrDashboardNotFound", err)
		}
	})

	t.Run("DashboardUpdatePreservesTilesAndWindow", func(t *testing.T) {
		if projectID == "" {
			t.Skip("skipping: CreateProject did not produce a project ID")
		}

		// Update returns the dashboard with its tiles so the RPC response is complete;
		// it also full-replaces the window and partial-updates the description.
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Original", "first desc",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboard.ID) })

		body := "tile body"
		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Tile", "",
			dashboards.MarkdownTile{Body: body},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile: %v", err)
		}

		// Empty description must be preserved (partial-update semantics); the window
		// is full-replaced.
		renamed, err := svc.UpdateDashboard(ctx, projectID, dashboard.ID, "Renamed", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS, insightsv1.Granularity_GRANULARITY_WEEK)
		if err != nil {
			t.Fatalf("UpdateDashboard: %v", err)
		}
		if renamed.Dashboard.DisplayName != "Renamed" {
			t.Errorf("DisplayName = %q, want %q", renamed.Dashboard.DisplayName, "Renamed")
		}
		if renamed.Dashboard.Description != "first desc" {
			t.Errorf("Description = %q, want %q (empty input should preserve)", renamed.Dashboard.Description, "first desc")
		}
		if renamed.Dashboard.DefaultTimeRange != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS.String() {
			t.Errorf("DefaultTimeRange = %q, want LAST_90_DAYS (full-replace)", renamed.Dashboard.DefaultTimeRange)
		}
		if renamed.Dashboard.DefaultGranularity != insightsv1.Granularity_GRANULARITY_WEEK.String() {
			t.Errorf("DefaultGranularity = %q, want WEEK (full-replace)", renamed.Dashboard.DefaultGranularity)
		}
		if len(renamed.Tiles) != 1 || renamed.Tiles[0].ID != tile.ID {
			t.Fatalf("Tiles = %+v, want single tile %s", renamed.Tiles, tile.ID)
		}

		// Non-empty description must overwrite.
		renamed2, err := svc.UpdateDashboard(ctx, projectID, dashboard.ID, "Renamed2", "new desc",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
		if err != nil {
			t.Fatalf("UpdateDashboard second: %v", err)
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
		projB, err := projectsSvc.CreateProject(ctx, org.ID, "Isolation B")
		if err != nil {
			t.Fatalf("CreateProject B: %v", err)
		}
		t.Cleanup(func() { _ = projectsSvc.DeleteProject(ctx, dbwrite.DeleteProjectParams{ID: projB.ID, OrgID: org.ID}) })

		dashboardA, err := svc.CreateDashboard(ctx, projectID, "Isolation A", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED)
		if err != nil {
			t.Fatalf("CreateDashboard A: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboardA.ID) })

		body := "tile in A"
		tileA, err := svc.CreateDashboardTile(ctx, projectID, dashboardA.ID, "Tile A", "",
			dashboards.MarkdownTile{Body: body},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile A: %v", err)
		}

		// Reads from project B must not see project A's dashboard.
		if _, err := svc.GetDashboard(ctx, projB.ID, dashboardA.ID); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("GetDashboard cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// Writes against the dashboard from project B must not succeed.
		if _, err := svc.UpdateDashboard(ctx, projB.ID, dashboardA.ID, "hijack", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("UpdateDashboard cross-project err = %v, want ErrDashboardNotFound", err)
		}
		if err := svc.DeleteDashboard(ctx, projB.ID, dashboardA.ID); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("DeleteDashboard cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// CreateDashboardTile against project A's dashboard from project B must
		// fail with ErrDashboardNotFound — the insert's WHERE clause depends on
		// the project_id filter joining dashboards.
		body2 := "hijack body"
		if _, err := svc.CreateDashboardTile(ctx, projB.ID, dashboardA.ID, "Hijack", "",
			dashboards.MarkdownTile{Body: body2},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil); !errors.Is(err, dashboards.ErrDashboardNotFound) {
			t.Errorf("CreateDashboardTile cross-project err = %v, want ErrDashboardNotFound", err)
		}

		// Tile-level writes from project B against project A's tile must surface
		// ErrDashboardTileNotFound (the join on dashboards.project_id eliminates
		// the row in the update's FROM clause).
		hijackBody := "hijack rename"
		if _, err := svc.UpdateDashboardTile(ctx, projB.ID, dashboardA.ID, tileA.ID, "hijack", "",
			dashboards.MarkdownTile{Body: hijackBody},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil); !errors.Is(err, dashboards.ErrDashboardTileNotFound) {
			t.Errorf("UpdateDashboardTile cross-project err = %v, want ErrDashboardTileNotFound", err)
		}
		if err := svc.DeleteDashboardTile(ctx, projB.ID, dashboardA.ID, tileA.ID); !errors.Is(err, dashboards.ErrDashboardTileNotFound) {
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

		dashboard, err := svc.CreateDashboard(ctx, projectID, "Conflict Dashboard", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED)
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
			dashboards.MarkdownTile{Body: bodyA},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil); err != nil {
			t.Fatalf("first titled tile: %v", err)
		}
		_, err = svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "notes", "",
			dashboards.MarkdownTile{Body: bodyB},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
		if !errors.Is(err, dashboards.ErrDashboardTileDisplayNameConflict) {
			t.Fatalf("second titled tile err = %v, want ErrDashboardTileDisplayNameConflict", err)
		}

		// Two untitled tiles in the same dashboard — both should succeed (partial-index exemption).
		if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "", "",
			dashboards.MarkdownTile{Body: bodyC},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil); err != nil {
			t.Fatalf("first untitled tile: %v", err)
		}
		if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "", "",
			dashboards.MarkdownTile{Body: bodyD},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil); err != nil {
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
			dashboards.MarkdownTile{Body: bodyD},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
		if !errors.Is(err, dashboards.ErrDashboardTileDisplayNameConflict) {
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
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Partial Update Dashboard", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED)
		if err != nil {
			t.Fatalf("CreateDashboard: %v", err)
		}
		t.Cleanup(func() { _ = svc.DeleteDashboard(ctx, projectID, dashboard.ID) })

		tile, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Original Title", "original desc",
			dashboards.MarkdownTile{Body: "body"},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
		if err != nil {
			t.Fatalf("CreateDashboardTile: %v", err)
		}

		// Update with empty DisplayName and Description — both must be preserved.
		preserved, err := svc.UpdateDashboardTile(ctx, projectID, dashboard.ID, tile.ID, "", "",
			dashboards.MarkdownTile{Body: "body"},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
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
			dashboards.MarkdownTile{Body: "body"},
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
			nil)
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
		dashboard, err := svc.CreateDashboard(ctx, projectID, "Empty Dashboard", "",
			commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, insightsv1.Granularity_GRANULARITY_UNSPECIFIED)
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
