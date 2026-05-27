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
// default window. Both are validated at the RPC boundary before reaching here, and
// both use their zero value to mean "no override" (resolveEffectiveWindow then
// falls back to the dashboard default).
type DashboardQueryOverrides struct {
	TimeRange   *commonv1.TimeRange    // nil = no override
	Granularity insightsv1.Granularity // GRANULARITY_UNSPECIFIED (zero) = no override
}

// RenderedTile is a tile plus, for insight tiles, its query outcome.
//
// Invariant, upheld by renderInsightTile (the sole producer): for an insight tile
// exactly one of Result / ErrorMessage is set, and Result is non-nil whenever
// ErrorMessage == ""; markdown tiles carry neither. renderedDashboardToRPC depends
// on this — it checks ErrorMessage before Result — and renderInsightTile's
// result == nil guard prevents an outcome-less insight tile. The proto RenderedTile
// models this as a oneof, but that is not validated outbound, so this Go-side
// discipline is the actual enforcement.
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
// tiles in dashboard order with markdown included. A per-tile query/validation
// failure populates that tile's ErrorMessage without failing the call. The only
// error returned is a request-level context cancellation/deadline: a tile
// goroutine returns it so the errgroup cancels the siblings and Wait surfaces it
// for the handler to map to the right status (rather than a 200 of "failed"
// tiles).
func RenderDashboard(
	ctx context.Context,
	executor *coreinsights.Executor,
	dashboard DashboardWithTiles,
	overrides DashboardQueryOverrides,
) (RenderedDashboard, error) {
	now := time.Now()
	timeRange, granularity := resolveEffectiveWindow(ctx, dashboard.Dashboard, overrides, now)

	rendered := make([]RenderedTile, len(dashboard.Tiles))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxConcurrentDashboardTileQueries)

	for i, tile := range dashboard.Tiles {
		rendered[i] = RenderedTile{Tile: tile}
		if TileKind(tile.Kind) != TileKindInsight {
			continue // markdown: structure only, no outcome
		}
		group.Go(func() error {
			result, errMsg, err := renderInsightTile(groupCtx, executor, dashboard.Dashboard.ProjectID, tile, timeRange, granularity, now)
			if err != nil {
				return err // context cancellation/deadline: cancel siblings, surface via Wait
			}
			rendered[i].Result = result
			rendered[i].ErrorMessage = errMsg
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return RenderedDashboard{}, err
	}

	return RenderedDashboard{Dashboard: dashboard.Dashboard, Tiles: rendered}, nil
}

// resolveEffectiveWindow picks the time range and granularity applied to every
// insight tile: request override wins, else the dashboard's stored default
// (normalized to LAST_30_DAYS / DAY when unset).
func resolveEffectiveWindow(ctx context.Context, dash dbread.Dashboard, overrides DashboardQueryOverrides, now time.Time) (*commonv1.TimeRange, insightsv1.Granularity) {
	timeRange := overrides.TimeRange
	if timeRange == nil {
		preset := DashboardDefaultTimeRangePresetFromDB(ctx, dash.DefaultTimeRange)
		timeRange = ResolveDashboardTimeRangePreset(preset, nil, now)
	}
	granularity := overrides.Granularity
	if granularity == insightsv1.Granularity_GRANULARITY_UNSPECIFIED {
		granularity = DashboardGranularityFromDB(ctx, dash.DefaultGranularity)
	}
	return timeRange, granularity
}

// renderInsightTile assembles a QueryRequest from the tile's stored spec plus the
// effective window, re-validates it (so the per-granularity range caps apply per
// tile), and executes it. Returns (result, "", nil) on success or (nil, message,
// nil) on a per-tile failure, where message is client-safe. A non-nil error is
// returned only for a request-level context cancellation/deadline, which the
// caller propagates instead of masking as a per-tile failure.
func renderInsightTile(
	ctx context.Context,
	executor *coreinsights.Executor,
	projectID string,
	tile dbread.DashboardTile,
	timeRange *commonv1.TimeRange,
	granularity insightsv1.Granularity,
	now time.Time,
) (*insightsv1.QueryResponse, string, error) {
	if len(tile.InsightQuery) == 0 {
		return nil, "insight tile is missing its query", nil
	}
	spec, err := MapToSpecMessage(tile.InsightQuery)
	if err != nil {
		slog.WarnContext(ctx, "dashboard tile query decode failed",
			slog.String("tile_id", tile.ID), slog.String("reason", err.Error()))
		return nil, "invalid query parameters: " + err.Error(), nil
	}

	assembled := &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   timeRange,
		Granularity: granularity.Enum(),
	}
	if err := protovalidate.Validate(assembled); err != nil {
		slog.WarnContext(ctx, "dashboard tile query invalid",
			slog.String("tile_id", tile.ID), slog.String("reason", err.Error()))
		return nil, "invalid query parameters: " + err.Error(), nil
	}

	result, err := coreinsights.ExecuteQuery(ctx, executor, projectID, assembled, now)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err // request lifecycle, not a tile fault — propagate
		}
		var invalid *coreinsights.InvalidQueryError
		if errors.As(err, &invalid) {
			return nil, "invalid query parameters: " + invalid.Message, nil
		}
		return nil, "query failed", nil
	}
	if result == nil {
		// ExecuteQuery returning (nil, nil) would violate the rendered_tile
		// "insight requires outcome" invariant (the response oneof is not
		// validated outbound). Guard so a future regression can't emit an
		// outcome-less insight tile.
		return nil, "query failed", nil
	}
	return result, "", nil
}
