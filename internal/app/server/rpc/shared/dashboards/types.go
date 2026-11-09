package dashboards

import (
	"google.golang.org/protobuf/proto"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	shareddashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

const displayColumns = 12

func tileToRPC(msg *dashboardsv1.DashboardTile) *shareddashboardsv1.Tile {
	tile := &shareddashboardsv1.Tile{
		Id:          proto.String(msg.GetId()),
		DisplayName: proto.String(msg.GetDisplayName()),
		Description: proto.String(msg.GetDescription()),
		Position:    positionToRPC(msg.GetPosition()),
	}
	switch content := msg.GetContent().(type) {
	case *dashboardsv1.DashboardTile_Insight:
		tile.Spec = content.Insight.GetSpec()
		tile.Chart = chartFromView(tile.Spec.GetInsightType(), msg.GetViewMode())
	case *dashboardsv1.DashboardTile_Markdown:
		tile.Markdown = proto.String(content.Markdown.GetBody())
	}
	return tile
}

// chartFromView is the chart of a trends or top K tile, or nil for any other tile
// and for a view that isn't a chart.
func chartFromView(insightType insightsv1.InsightType, view dashboardsv1.DashboardTileViewMode) *shareddashboardsv1.Chart {
	if insightType != insightsv1.InsightType_INSIGHT_TYPE_TRENDS && insightType != insightsv1.InsightType_INSIGHT_TYPE_TOP_K {
		return nil
	}
	switch view {
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE:
		return shareddashboardsv1.Chart_CHART_LINE.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA:
		return shareddashboardsv1.Chart_CHART_AREA.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED:
		return shareddashboardsv1.Chart_CHART_BAR.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED:
		return shareddashboardsv1.Chart_CHART_STACKED_BAR.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_PIE:
		return shareddashboardsv1.Chart_CHART_PIE.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE:
		return shareddashboardsv1.Chart_CHART_TABLE.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI:
		return shareddashboardsv1.Chart_CHART_KPI.Enum()
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_MAP:
	}
	return nil
}

// viewModeFromChart is unspecified for an unset chart, which keeps the stored view.
func viewModeFromChart(chart shareddashboardsv1.Chart) dashboardsv1.DashboardTileViewMode {
	switch chart {
	case shareddashboardsv1.Chart_CHART_LINE:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
	case shareddashboardsv1.Chart_CHART_AREA:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
	case shareddashboardsv1.Chart_CHART_BAR:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED
	case shareddashboardsv1.Chart_CHART_STACKED_BAR:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED
	case shareddashboardsv1.Chart_CHART_PIE:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_PIE
	case shareddashboardsv1.Chart_CHART_TABLE:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE
	case shareddashboardsv1.Chart_CHART_KPI:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI
	}
	return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
}

// positionToRPC shows a stored position on the 12-column grid the way the app
// does (storedPositionToDisplay), and a tile with none at the app's fallback.
func positionToRPC(p *dashboardsv1.GridPosition) *shareddashboardsv1.Position {
	if p == nil {
		return &shareddashboardsv1.Position{X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(18)}
	}
	x := min(max(columns(p.GetX()), 0), displayColumns-1)
	right := max(x+1, min(displayColumns, columns(max(p.GetX()+1, p.GetX()+p.GetW()))))
	return &shareddashboardsv1.Position{X: proto.Int32(x), Y: proto.Int32(p.GetY()), W: proto.Int32(right - x), H: proto.Int32(p.GetH())}
}

// columns is Math.round(units / 6) for the non-negative units the grid stores.
func columns(units int32) int32 {
	return (units + coredashboards.UnitsPerColumn/2) / coredashboards.UnitsPerColumn
}

func positionToStored(p *shareddashboardsv1.Position) *dashboardsv1.GridPosition {
	if p == nil {
		return nil
	}
	return &dashboardsv1.GridPosition{
		X: proto.Int32(p.GetX() * coredashboards.UnitsPerColumn),
		Y: proto.Int32(p.GetY()),
		W: proto.Int32(p.GetW() * coredashboards.UnitsPerColumn),
		H: proto.Int32(p.GetH()),
	}
}

// chosen treats an unspecified enum like an unset one: keep the stored value, or
// take the default on create.
func chosen[E ~int32](e *E) *E {
	if e == nil || *e == 0 {
		return nil
	}
	return e
}
