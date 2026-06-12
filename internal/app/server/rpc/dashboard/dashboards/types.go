package dashboards

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func roDashboardToRPC(ctx context.Context, dashboard coredashboards.DashboardWithTiles) (*dashboardsv1.Dashboard, error) {
	tiles := make([]*dashboardsv1.DashboardTile, 0, len(dashboard.Tiles))
	for _, tile := range dashboard.Tiles {
		msg, err := coredashboards.TileToRPC(ctx, tile)
		if err != nil {
			return nil, fmt.Errorf("tile %s: %w", tile.ID, err)
		}
		tiles = append(tiles, msg)
	}
	out := &dashboardsv1.Dashboard{
		Id:                 proto.String(dashboard.Dashboard.ID),
		ProjectId:          proto.String(dashboard.Dashboard.ProjectID),
		DisplayName:        proto.String(dashboard.Dashboard.DisplayName),
		Description:        proto.String(dashboard.Dashboard.Description),
		CreateTime:         toTimestamp(dashboard.Dashboard.CreateTime.Time),
		UpdateTime:         toTimestamp(dashboard.Dashboard.UpdateTime.Time),
		Tiles:              tiles,
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(ctx, dashboard.Dashboard.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(ctx, dashboard.Dashboard.DefaultGranularity).Enum(),
	}
	// Share is non-nil only when enabled (DashboardWithTiles invariant), so a
	// presence check is sufficient — no need to re-check Enabled here.
	if dashboard.Share != nil {
		out.ShareId = proto.String(dashboard.Share.ShareToken)
	}
	return out, nil
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
			Compare:       in.GetCompare(),
			Thresholds:    in.GetThresholds(),
			Header:        in.GetHeader(),
			Visualization: in.GetVisualization(),
			Position:      in.GetPosition(),
		},
	}, nil
}

func toTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
