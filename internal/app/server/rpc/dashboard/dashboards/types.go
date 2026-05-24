package dashboards

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func roDashboardToRPC(dashboard coredashboards.DashboardWithTiles) (*dashboardsv1.Dashboard, error) {
	tiles := make([]*dashboardsv1.DashboardTile, 0, len(dashboard.Tiles))
	for _, tile := range dashboard.Tiles {
		msg, err := roTileToRPC(tile)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, msg)
	}
	return &dashboardsv1.Dashboard{
		Id:                 proto.String(dashboard.Dashboard.ID),
		ProjectId:          proto.String(dashboard.Dashboard.ProjectID),
		DisplayName:        proto.String(dashboard.Dashboard.DisplayName),
		Description:        proto.String(dashboard.Dashboard.Description),
		CreateTime:         toTimestamp(dashboard.Dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(dashboard.Dashboard.UpdateTime.Time),
		Tiles:              tiles,
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(dashboard.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(dashboard.Dashboard.DefaultGranularity).Enum(),
	}, nil
}

// wDashboardToRPC encodes a freshly-created dashboard. The Tiles slice is
// intentionally absent — a brand-new dashboard has no tiles.
func wDashboardToRPC(dashboard dbwrite.Dashboard) *dashboardsv1.Dashboard {
	return &dashboardsv1.Dashboard{
		Id:                 proto.String(dashboard.ID),
		ProjectId:          proto.String(dashboard.ProjectID),
		DisplayName:        proto.String(dashboard.DisplayName),
		Description:        proto.String(dashboard.Description),
		CreateTime:         toTimestamp(dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(dashboard.UpdateTime.Time),
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(dashboard.DefaultGranularity).Enum(),
	}
}

func renderedDashboardToRPC(rd coredashboards.RenderedDashboard) (*dashboardsv1.RenderedDashboard, error) {
	tiles := make([]*dashboardsv1.RenderedTile, 0, len(rd.Tiles))
	for _, rt := range rd.Tiles {
		tileMsg, err := roTileToRPC(rt.Tile)
		if err != nil {
			return nil, err
		}
		msg := &dashboardsv1.RenderedTile{Tile: tileMsg}
		switch {
		case rt.ErrorMessage != "":
			msg.Outcome = &dashboardsv1.RenderedTile_ErrorMessage{ErrorMessage: rt.ErrorMessage}
		case rt.Result != nil:
			msg.Outcome = &dashboardsv1.RenderedTile_Result{Result: rt.Result}
		}
		tiles = append(tiles, msg)
	}
	return &dashboardsv1.RenderedDashboard{
		Id:                 proto.String(rd.Dashboard.ID),
		DisplayName:        proto.String(rd.Dashboard.DisplayName),
		Description:        proto.String(rd.Dashboard.Description),
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(rd.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(rd.Dashboard.DefaultGranularity).Enum(),
		CreateTime:         toTimestamp(rd.Dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(rd.Dashboard.UpdateTime.Time),
		Tiles:              tiles,
	}, nil
}

func roTileToRPC(tile dbread.DashboardTile) (*dashboardsv1.DashboardTile, error) {
	layouts, err := coredashboards.MapToLayouts(tile.Layouts)
	if err != nil {
		return nil, err
	}
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		Layouts:     layouts,
		CreateTime:  toTimestamp(tile.CreateTime.Time),
		UpdateTime:  toTimestamp(tile.UpdateTime.Time),
		ViewMode:    tileViewModeToRPC(coredashboards.TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if err := setTileContent(msg, tile.ID, coredashboards.TileKind(tile.Kind), tile.InsightQuery, tile.MarkdownBody.String, tile.MarkdownBody.Valid); err != nil {
		return nil, err
	}
	return msg, nil
}

func wTileToRPC(tile dbwrite.DashboardTile) (*dashboardsv1.DashboardTile, error) {
	layouts, err := coredashboards.MapToLayouts(tile.Layouts)
	if err != nil {
		return nil, err
	}
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		Layouts:     layouts,
		CreateTime:  toTimestamp(tile.CreateTime.Time),
		UpdateTime:  toTimestamp(tile.UpdateTime.Time),
		ViewMode:    tileViewModeToRPC(coredashboards.TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if err := setTileContent(msg, tile.ID, coredashboards.TileKind(tile.Kind), tile.InsightQuery, tile.MarkdownBody.String, tile.MarkdownBody.Valid); err != nil {
		return nil, err
	}
	return msg, nil
}

// setTileContent populates the tile's content oneof from the raw DB columns,
// verifying the dashboard_tiles_kind_payload CHECK invariants. The CHECK
// constraint guarantees the appropriate payload column is non-NULL for each
// kind, so the missing-payload branches only trip on data corruption or
// manual DB tinkering — but failing loudly is safer than encoding garbage.
func setTileContent(msg *dashboardsv1.DashboardTile, tileID string, kind coredashboards.TileKind, insightQuery map[string]any, markdownBody string, markdownValid bool) error {
	switch kind {
	case coredashboards.TileKindInsight:
		if len(insightQuery) == 0 {
			return fmt.Errorf("tile %s: insight tile row missing query", tileID)
		}
		spec, err := coredashboards.MapToSpecMessage(insightQuery)
		if err != nil {
			return err
		}
		msg.Content = &dashboardsv1.DashboardTile_Insight{
			Insight: &dashboardsv1.InsightTileContent{Spec: spec},
		}
		return nil
	case coredashboards.TileKindMarkdown:
		if !markdownValid {
			return fmt.Errorf("tile %s: markdown tile row missing body", tileID)
		}
		msg.Content = &dashboardsv1.DashboardTile_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(markdownBody)},
		}
		return nil
	default:
		return fmt.Errorf("tile %s: unknown tile kind %d", tileID, kind)
	}
}

func tileViewModeToRPC(kind coredashboards.TileKind, raw string) dashboardsv1.DashboardTileViewMode {
	switch kind {
	case coredashboards.TileKindInsight:
		value, ok := dashboardsv1.DashboardTileViewMode_value[raw]
		if !ok {
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
		}
		switch dashboardsv1.DashboardTileViewMode(value) {
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
		default:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
		}
	case coredashboards.TileKindMarkdown:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	default:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	}
}

func tileContentFromCreateRPC(c any) (coredashboards.TileContent, error) {
	switch v := c.(type) {
	case *dashboardsv1.DashboardsServiceCreateTileRequest_Insight:
		return coredashboards.InsightTile{Spec: v.Insight.GetSpec()}, nil
	case *dashboardsv1.DashboardsServiceCreateTileRequest_Markdown:
		return coredashboards.MarkdownTile{Body: v.Markdown.GetBody()}, nil
	default:
		return nil, errors.New("unknown tile content")
	}
}

func tileContentFromUpdateRPC(c any) (coredashboards.TileContent, error) {
	switch v := c.(type) {
	case *dashboardsv1.DashboardsServiceUpdateTileRequest_Insight:
		return coredashboards.InsightTile{Spec: v.Insight.GetSpec()}, nil
	case *dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown:
		return coredashboards.MarkdownTile{Body: v.Markdown.GetBody()}, nil
	default:
		return nil, errors.New("unknown tile content")
	}
}

func toTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
