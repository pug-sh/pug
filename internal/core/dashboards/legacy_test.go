package dashboards_test

import (
	"context"

	"github.com/pug-sh/pug/internal/core/dashboards"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// createTileLegacy adapts the pre-TilePayload positional-arg call shape used
// throughout these tests to the current TilePayload-based service signature.
// New production call sites construct TilePayload directly.
func createTileLegacy(ctx context.Context, svc *dashboards.Service, projectID, dashboardID, displayName, description string, content dashboards.TileContent, viewMode dashboardsv1.DashboardTileViewMode, layouts []*dashboardsv1.ResponsiveGridLayout) (dbwrite.DashboardTile, error) {
	return svc.CreateDashboardTile(ctx, projectID, dashboardID, dashboards.TilePayload{
		DisplayName: displayName,
		Description: description,
		Content:     content,
		ViewMode:    viewMode,
		Layouts:     layouts,
	})
}

func updateTileLegacy(ctx context.Context, svc *dashboards.Service, projectID, dashboardID, tileID, displayName, description string, content dashboards.TileContent, viewMode dashboardsv1.DashboardTileViewMode, layouts []*dashboardsv1.ResponsiveGridLayout) (dbwrite.DashboardTile, error) {
	return svc.UpdateDashboardTile(ctx, projectID, dashboardID, tileID, dashboards.TilePayload{
		DisplayName: displayName,
		Description: description,
		Content:     content,
		ViewMode:    viewMode,
		Layouts:     layouts,
	})
}
