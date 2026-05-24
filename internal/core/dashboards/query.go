package dashboards

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"golang.org/x/sync/errgroup"

	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

const maxConcurrentDashboardTileQueries = 8

// DashboardQueryOverrides carries optional view-time overrides of the dashboard
// default window. Both are validated at the RPC boundary before reaching here.
type DashboardQueryOverrides struct {
	TimeRange   *commonv1.TimeRange
	Granularity insightsv1.Granularity
}

// RenderedTile is a tile plus, for insight tiles, its query outcome. Markdown
// tiles carry an empty Result and ErrorMessage.
type RenderedTile struct {
	Tile         dbread.DashboardTile
	Result       *insightsv1.QueryResponse
	ErrorMessage string
}

// RenderedDashboard is a dashboard row plus its tiles rendered in dashboard order.
type RenderedDashboard struct {
	Dashboard dbread.Dashboard
	Tiles     []RenderedTile
}

// RenderDashboard executes every insight tile against the dashboard's effective
// window (request override → dashboard default), resolved once, and returns all
// tiles in dashboard order with markdown included. Per-tile failures populate
// ErrorMessage; the call never fails wholesale.
func RenderDashboard(
	ctx context.Context,
	executor *coreinsights.Executor,
	dashboard DashboardWithTiles,
	overrides DashboardQueryOverrides,
) RenderedDashboard {
	now := time.Now()
	timeRange, granularity := resolveEffectiveWindow(dashboard.Dashboard, overrides, now)

	rendered := make([]RenderedTile, len(dashboard.Tiles))
	sem := make(chan struct{}, maxConcurrentDashboardTileQueries)
	group, groupCtx := errgroup.WithContext(ctx)

	for i, tile := range dashboard.Tiles {
		rendered[i] = RenderedTile{Tile: tile}
		if TileKind(tile.Kind) != TileKindInsight {
			continue // markdown: structure only, no outcome
		}
		group.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()
			result, errMsg := renderInsightTile(groupCtx, executor, dashboard.Dashboard.ProjectID, tile, timeRange, granularity)
			rendered[i].Result = result
			rendered[i].ErrorMessage = errMsg
			return nil
		})
	}
	_ = group.Wait()

	return RenderedDashboard{Dashboard: dashboard.Dashboard, Tiles: rendered}
}

// resolveEffectiveWindow picks the time range and granularity applied to every
// insight tile: request override wins, else the dashboard's stored default
// (normalized to LAST_30_DAYS / DAY when unset).
func resolveEffectiveWindow(dash dbread.Dashboard, overrides DashboardQueryOverrides, now time.Time) (*commonv1.TimeRange, insightsv1.Granularity) {
	timeRange := overrides.TimeRange
	if timeRange == nil {
		preset := DashboardDefaultTimeRangePresetFromDB(dash.DefaultTimeRange)
		timeRange = ResolveDashboardTimeRangePreset(preset, nil, now)
	}
	granularity := overrides.Granularity
	if granularity == insightsv1.Granularity_GRANULARITY_UNSPECIFIED {
		granularity = DashboardGranularityFromDB(dash.DefaultGranularity)
	}
	return timeRange, granularity
}

// renderInsightTile assembles a QueryRequest from the tile's stored spec plus the
// effective window, re-validates it (so the per-granularity range caps apply per
// tile), and executes it. Returns (result, "") on success or (nil, message) on a
// per-tile failure, where message is client-safe.
func renderInsightTile(
	ctx context.Context,
	executor *coreinsights.Executor,
	projectID string,
	tile dbread.DashboardTile,
	timeRange *commonv1.TimeRange,
	granularity insightsv1.Granularity,
) (*insightsv1.QueryResponse, string) {
	if len(tile.InsightQuery) == 0 {
		return nil, "insight tile is missing its query"
	}
	spec, err := MapToSpecMessage(tile.InsightQuery)
	if err != nil {
		slog.WarnContext(ctx, "dashboard tile query decode failed",
			slog.String("tile_id", tile.ID), slog.String("reason", err.Error()))
		return nil, "invalid query parameters: " + err.Error()
	}

	assembled := &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   timeRange,
		Granularity: granularity.Enum(),
	}
	if err := protovalidate.Validate(assembled); err != nil {
		slog.WarnContext(ctx, "dashboard tile query invalid",
			slog.String("tile_id", tile.ID), slog.String("reason", err.Error()))
		return nil, "invalid query parameters: " + err.Error()
	}

	result, err := coreinsights.ExecuteQuery(ctx, executor, projectID, assembled)
	if err != nil {
		var invalid *coreinsights.InvalidQueryError
		if errors.As(err, &invalid) {
			return nil, "invalid query parameters: " + invalid.Message
		}
		return nil, "query failed"
	}
	return result, ""
}
