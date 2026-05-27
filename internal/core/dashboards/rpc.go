package dashboards

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// RenderedDashboardToRPC encodes a rendered dashboard for RPC responses.
func RenderedDashboardToRPC(ctx context.Context, rd RenderedDashboard) *dashboardsv1.RenderedDashboard {
	tiles := make([]*dashboardsv1.RenderedTile, 0, len(rd.Tiles))
	for _, rt := range rd.Tiles {
		tiles = append(tiles, renderedTileToRPC(ctx, rt))
	}
	return &dashboardsv1.RenderedDashboard{
		Id:                 proto.String(rd.Dashboard.ID),
		DisplayName:        proto.String(rd.Dashboard.DisplayName),
		Description:        proto.String(rd.Dashboard.Description),
		DefaultTimeRange:   DashboardDefaultTimeRangePresetFromDB(ctx, rd.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: DashboardGranularityFromDB(ctx, rd.Dashboard.DefaultGranularity).Enum(),
		CreateTime:         timestampToRPC(rd.Dashboard.CreateTime.Time),
		UpdateTime:         timestampToRPC(rd.Dashboard.UpdateTime.Time),
		Tiles:              tiles,
	}
}

// renderedTileToRPC encodes one rendered tile. If the stored tile row can't be
// decoded by tileToRPC (corrupt payload / cross-deploy schema drift), it records
// the data-integrity error server-side and degrades to a structural tile carrying an
// error_message outcome — a corrupt tile must not fail the whole QueryDashboard, the
// same per-tile contract renderInsightTile upholds at execution time.
func renderedTileToRPC(ctx context.Context, rt RenderedTile) *dashboardsv1.RenderedTile {
	tileMsg, err := tileToRPC(rt.Tile)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode rendered dashboard tile",
			slogx.Error(err), slog.String("tile_id", rt.Tile.ID))
		telemetry.RecordError(ctx, err)
		// Reuse the render-phase message when present (e.g. "invalid query
		// parameters: …"); otherwise a generic, client-safe message. The internal
		// encode error is logged above, never surfaced to the client.
		errMsg := rt.ErrorMessage
		if errMsg == "" {
			errMsg = "tile could not be rendered"
		}
		return &dashboardsv1.RenderedTile{
			Tile:    structuralTileToRPC(rt.Tile),
			Outcome: &dashboardsv1.RenderedTile_ErrorMessage{ErrorMessage: errMsg},
		}
	}
	msg := &dashboardsv1.RenderedTile{Tile: tileMsg}
	switch {
	case rt.ErrorMessage != "":
		msg.Outcome = &dashboardsv1.RenderedTile_ErrorMessage{ErrorMessage: rt.ErrorMessage}
	case rt.Result != nil:
		msg.Outcome = &dashboardsv1.RenderedTile_Result{Result: rt.Result}
	}
	return msg
}

// structuralTileToRPC builds a tile message with identity, timestamps, and (best
// effort) layout but no content oneof — for the degraded path where the content
// payload can't be decoded. Layouts are attempted but omitted on their own failure
// (the primary error is already recorded by the caller).
func structuralTileToRPC(tile dbread.DashboardTile) *dashboardsv1.DashboardTile {
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		CreateTime:  timestampToRPC(tile.CreateTime.Time),
		UpdateTime:  timestampToRPC(tile.UpdateTime.Time),
		ViewMode:    tileViewModeToRPC(TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if layouts, err := MapToLayouts(tile.Layouts); err == nil {
		msg.Layouts = layouts
	}
	return msg
}

func tileToRPC(tile dbread.DashboardTile) (*dashboardsv1.DashboardTile, error) {
	layouts, err := MapToLayouts(tile.Layouts)
	if err != nil {
		return nil, err
	}
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		Layouts:     layouts,
		CreateTime:  timestampToRPC(tile.CreateTime.Time),
		UpdateTime:  timestampToRPC(tile.UpdateTime.Time),
		ViewMode:    tileViewModeToRPC(TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if err := setTileContent(msg, tile.ID, TileKind(tile.Kind), tile.InsightQuery, tile.MarkdownBody.String, tile.MarkdownBody.Valid); err != nil {
		return nil, err
	}
	return msg, nil
}

func setTileContent(msg *dashboardsv1.DashboardTile, tileID string, kind TileKind, insightQuery map[string]any, markdownBody string, markdownValid bool) error {
	switch kind {
	case TileKindInsight:
		if len(insightQuery) == 0 {
			return fmt.Errorf("tile %s: insight tile row missing query", tileID)
		}
		spec, err := MapToSpecMessage(insightQuery)
		if err != nil {
			return err
		}
		msg.Content = &dashboardsv1.DashboardTile_Insight{
			Insight: &dashboardsv1.InsightTileContent{Spec: spec},
		}
		return nil
	case TileKindMarkdown:
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

func tileViewModeToRPC(kind TileKind, raw string) dashboardsv1.DashboardTileViewMode {
	switch kind {
	case TileKindInsight:
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
	case TileKindMarkdown:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	default:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	}
}

func timestampToRPC(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
