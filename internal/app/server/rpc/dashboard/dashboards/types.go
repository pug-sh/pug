package dashboards

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

func roDashboardToRPC(ctx context.Context, dashboard coredashboards.DashboardWithTiles) (*dashboardsv1.Dashboard, error) {
	tiles := make([]*dashboardsv1.DashboardTile, 0, len(dashboard.Tiles))
	for _, tile := range dashboard.Tiles {
		msg, err := roTileToRPC(ctx, tile)
		if err != nil {
			return nil, fmt.Errorf("tile %s: %w", tile.ID, err)
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
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(ctx, dashboard.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(ctx, dashboard.Dashboard.DefaultGranularity).Enum(),
	}, nil
}

// wDashboardToRPC encodes a freshly-created dashboard. The Tiles slice is
// intentionally absent — a brand-new dashboard has no tiles.
func wDashboardToRPC(ctx context.Context, dashboard dbwrite.Dashboard) *dashboardsv1.Dashboard {
	return &dashboardsv1.Dashboard{
		Id:                 proto.String(dashboard.ID),
		ProjectId:          proto.String(dashboard.ProjectID),
		DisplayName:        proto.String(dashboard.DisplayName),
		Description:        proto.String(dashboard.Description),
		CreateTime:         toTimestamp(dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(dashboard.UpdateTime.Time),
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(ctx, dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(ctx, dashboard.DefaultGranularity).Enum(),
	}
}

func renderedDashboardToRPC(ctx context.Context, rd coredashboards.RenderedDashboard) *dashboardsv1.RenderedDashboard {
	tiles := make([]*dashboardsv1.RenderedTile, 0, len(rd.Tiles))
	for _, rt := range rd.Tiles {
		tiles = append(tiles, renderedTileToRPC(ctx, rt))
	}
	return &dashboardsv1.RenderedDashboard{
		Id:                 proto.String(rd.Dashboard.ID),
		DisplayName:        proto.String(rd.Dashboard.DisplayName),
		Description:        proto.String(rd.Dashboard.Description),
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(ctx, rd.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(ctx, rd.Dashboard.DefaultGranularity).Enum(),
		CreateTime:         toTimestamp(rd.Dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(rd.Dashboard.UpdateTime.Time),
		Tiles:              tiles,
	}
}

// renderedTileToRPC encodes one rendered tile. If the stored tile row can't be
// decoded by roTileToRPC (corrupt payload / cross-deploy schema drift), it records
// the data-integrity error server-side and degrades to a structural tile carrying an
// error_message outcome — a corrupt tile must not fail the whole QueryDashboard, the
// same per-tile contract renderInsightTile upholds at execution time.
func renderedTileToRPC(ctx context.Context, rt coredashboards.RenderedTile) *dashboardsv1.RenderedTile {
	tileMsg, err := roTileToRPC(ctx, rt.Tile)
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
			Tile:    structuralTileToRPC(ctx, rt.Tile),
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
// payload can't be decoded. The primary error is already recorded by the caller;
// a secondary layouts decode failure is a distinct corruption and gets its own
// log line so we don't lose the signal.
func structuralTileToRPC(ctx context.Context, tile dbread.DashboardTile) *dashboardsv1.DashboardTile {
	msg := &dashboardsv1.DashboardTile{
		Id:          proto.String(tile.ID),
		DashboardId: proto.String(tile.DashboardID),
		DisplayName: proto.String(tile.DisplayName),
		Description: proto.String(tile.Description),
		CreateTime:  toTimestamp(tile.CreateTime.Time),
		UpdateTime:  toTimestamp(tile.UpdateTime.Time),
		ViewMode:    tileViewModeToRPC(ctx, coredashboards.TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	layouts, err := coredashboards.MapToLayouts(tile.Layouts)
	if err != nil {
		slog.WarnContext(ctx, "degraded tile: layouts also undecodable",
			slogx.Error(err), slog.String("tile_id", tile.ID))
		telemetry.RecordError(ctx, err)
		return msg
	}
	msg.Layouts = layouts
	return msg
}

func roTileToRPC(ctx context.Context, tile dbread.DashboardTile) (*dashboardsv1.DashboardTile, error) {
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
		ViewMode:    tileViewModeToRPC(ctx, coredashboards.TileKind(tile.Kind), tile.ViewMode).Enum(),
	}
	if err := setTileContent(msg, tile.ID, coredashboards.TileKind(tile.Kind), tile.InsightQuery, tile.MarkdownBody.String, tile.MarkdownBody.Valid); err != nil {
		return nil, err
	}
	if err := setTileCustomization(ctx, msg, tile.Compare, tile.Thresholds, tile.Header, tile.Visualization); err != nil {
		return nil, err
	}
	return msg, nil
}

// setTileCustomization populates compare / thresholds / header / visualization
// on the response from the DB row's stored columns. Errors propagate proto
// decoding failures (data corruption / schema drift). On the QueryDashboard
// path, renderedTileToRPC catches the error and degrades to a per-tile
// error_message; on Get / Update / Upsert / List (roDashboardToRPC), the error
// fails the whole response with CodeInternal — roDashboardToRPC wraps it with
// the failing tile id (fmt.Errorf "tile %s: %w") so the operator has a
// starting point in the recorded telemetry.
func setTileCustomization(ctx context.Context, msg *dashboardsv1.DashboardTile, compare string, thresholds []byte, header, visualization map[string]any) error {
	msg.Compare = coredashboards.ComparePeriodFromDB(ctx, compare).Enum()

	rules, err := coredashboards.UnmarshalThresholds(thresholds)
	if err != nil {
		return fmt.Errorf("unmarshal thresholds: %w", err)
	}
	msg.Thresholds = rules

	if len(header) > 0 {
		var h dashboardsv1.TileHeader
		if err := coredashboards.MapToMessage(header, &h); err != nil {
			return fmt.Errorf("decode header: %w", err)
		}
		msg.Header = &h
	}
	if len(visualization) > 0 {
		var v dashboardsv1.VisualizationOptions
		if err := coredashboards.MapToMessage(visualization, &v); err != nil {
			return fmt.Errorf("decode visualization: %w", err)
		}
		msg.Visualization = &v
	}
	return nil
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

func tileViewModeToRPC(ctx context.Context, kind coredashboards.TileKind, raw string) dashboardsv1.DashboardTileViewMode {
	switch kind {
	case coredashboards.TileKindInsight:
		value, ok := dashboardsv1.DashboardTileViewMode_value[raw]
		if !ok {
			if raw != "" {
				coredashboards.LogUnknownEnumOnce(ctx, "DashboardTileViewMode", "dashboard_tiles.view_mode", raw)
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

// upsertTileInputFromRPC translates a single proto DashboardTileInput to its
// service-layer counterpart. The content oneof discriminator drives the
// TileContent variant; protovalidate has already enforced oneof.required, so
// the default branch only trips on schema drift (a new content kind added to
// proto without an update here).
func upsertTileInputFromRPC(in *dashboardsv1.DashboardTileInput) (coredashboards.UpsertTileInput, error) {
	if in == nil {
		return coredashboards.UpsertTileInput{}, errors.New("nil tile input")
	}
	var content coredashboards.TileContent
	switch v := in.GetContent().(type) {
	case *dashboardsv1.DashboardTileInput_Insight:
		content = coredashboards.InsightTile{Spec: v.Insight.GetSpec()}
	case *dashboardsv1.DashboardTileInput_Markdown:
		content = coredashboards.MarkdownTile{Body: v.Markdown.GetBody()}
	default:
		return coredashboards.UpsertTileInput{}, errors.New("unknown tile content")
	}
	return coredashboards.UpsertTileInput{
		ID: in.GetId(),
		Payload: coredashboards.TilePayload{
			DisplayName:   in.GetDisplayName(),
			Description:   in.GetDescription(),
			Content:       content,
			ViewMode:      in.GetViewMode(),
			Layouts:       in.GetLayouts(),
			Compare:       in.GetCompare(),
			Thresholds:    in.GetThresholds(),
			Header:        in.GetHeader(),
			Visualization: in.GetVisualization(),
		},
	}, nil
}

func toTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
