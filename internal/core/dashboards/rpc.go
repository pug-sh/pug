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
// decoded by TileToRPC (corrupt payload / cross-deploy schema drift), it records
// the data-integrity error server-side and degrades to a structural tile carrying an
// error_message outcome — a corrupt tile must not fail the whole QueryDashboard, the
// same per-tile contract renderInsightTile upholds at execution time.
func renderedTileToRPC(ctx context.Context, rt RenderedTile) *dashboardsv1.RenderedTile {
	tileMsg, err := TileToRPC(ctx, rt.Tile)
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
			Tile:    StructuralTileToRPC(ctx, rt.Tile),
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

// StructuralTileToRPC builds a tile message with identity, timestamps, and (best
// effort) position but no content oneof — for the degraded path where the content
// payload can't be decoded. The primary error is already recorded by the caller;
// a secondary position decode failure is a distinct corruption and gets its own
// log line so we don't lose the signal.
func StructuralTileToRPC(ctx context.Context, tile dbread.DashboardTile) *dashboardsv1.DashboardTile {
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		CreateTime:  timestampToRPC(tile.CreateTime.Time),
		UpdateTime:  timestampToRPC(tile.UpdateTime.Time),
		ViewMode:    TileViewModeToRPC(ctx, TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if len(tile.Position) > 0 {
		var pos dashboardsv1.GridPosition
		if err := MapToMessage(tile.Position, &pos); err != nil {
			slog.WarnContext(ctx, "degraded tile: position also undecodable",
				slogx.Error(err), slog.String("tile_id", tile.ID))
			telemetry.RecordError(ctx, err)
			return msg
		}
		msg.Position = &pos
	}
	return msg
}

// TileToRPC encodes a stored dashboard tile row for RPC responses.
func TileToRPC(ctx context.Context, tile dbread.DashboardTile) (*dashboardsv1.DashboardTile, error) {
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		CreateTime:  timestampToRPC(tile.CreateTime.Time),
		UpdateTime:  timestampToRPC(tile.UpdateTime.Time),
		ViewMode:    TileViewModeToRPC(ctx, TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if err := SetTileContent(msg, tile.ID, TileKind(tile.Kind), tile.InsightQuery, tile.MarkdownBody.String, tile.MarkdownBody.Valid); err != nil {
		return nil, err
	}
	if err := setTileCustomization(ctx, msg, tile.Compare, tile.Thresholds, tile.Header, tile.Visualization, tile.Position); err != nil {
		return nil, err
	}
	return msg, nil
}

// setTileCustomization populates compare / thresholds / header / visualization /
// position on the response from the DB row's stored columns. Errors propagate proto
// decoding failures (data corruption / schema drift). On the QueryDashboard
// path, renderedTileToRPC catches the error and degrades to a per-tile
// error_message; on Get / Update / Upsert / List, the error fails the whole
// response — callers wrap it with the failing tile id.
func setTileCustomization(ctx context.Context, msg *dashboardsv1.DashboardTile, compare string, thresholds []byte, header, visualization, position map[string]any) error {
	msg.Compare = ComparePeriodFromDB(ctx, compare).Enum()

	rules, err := UnmarshalThresholds(thresholds)
	if err != nil {
		return fmt.Errorf("unmarshal thresholds: %w", err)
	}
	msg.Thresholds = rules

	if len(header) > 0 {
		var h dashboardsv1.TileHeader
		if err := MapStoredMessage(ctx, "dashboard_tiles.header", header, &h); err != nil {
			return fmt.Errorf("decode header: %w", err)
		}
		msg.Header = &h
	}
	if len(visualization) > 0 {
		var v dashboardsv1.VisualizationOptions
		if err := MapStoredMessage(ctx, "dashboard_tiles.visualization", visualization, &v); err != nil {
			return fmt.Errorf("decode visualization: %w", err)
		}
		msg.Visualization = &v
	}
	if len(position) > 0 {
		var pos dashboardsv1.GridPosition
		if err := MapStoredMessage(ctx, "dashboard_tiles.position", position, &pos); err != nil {
			return fmt.Errorf("decode position: %w", err)
		}
		msg.Position = &pos
	}
	return nil
}

// SetTileContent populates the tile's content oneof from the raw DB columns,
// verifying the dashboard_tiles_kind_payload CHECK invariants. The CHECK
// constraint guarantees the appropriate payload column is non-NULL for each
// kind, so the missing-payload branches only trip on data corruption or
// manual DB tinkering — but failing loudly is safer than encoding garbage.
func SetTileContent(msg *dashboardsv1.DashboardTile, tileID string, kind TileKind, insightQuery map[string]any, markdownBody string, markdownValid bool) error {
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

// TileViewModeToRPC maps a stored view_mode string to the proto enum for the
// given tile kind.
func TileViewModeToRPC(ctx context.Context, kind TileKind, raw string) dashboardsv1.DashboardTileViewMode {
	switch kind {
	case TileKindInsight:
		value, ok := dashboardsv1.DashboardTileViewMode_value[raw]
		if !ok {
			if raw != "" {
				LogUnknownEnumOnce(ctx, "DashboardTileViewMode", "dashboard_tiles.view_mode", raw)
			}
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
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY
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
