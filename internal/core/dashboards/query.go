package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

const maxConcurrentDashboardTileQueries = 8

// DashboardQueryOverrides carries optional dashboard-level query overrides.
type DashboardQueryOverrides struct {
	TimeRange   *commonv1.TimeRange
	Granularity insightsv1.Granularity
}

// DashboardTileQueryOutcome is the per-tile result of a dashboard batch query.
type DashboardTileQueryOutcome struct {
	TileID       string
	Result       *insightsv1.QueryResponse
	ErrorMessage string
}

// QueryDashboardTiles executes insight queries for all insight tiles on a dashboard.
// Results preserve dashboard tile order; markdown tiles are omitted.
func QueryDashboardTiles(
	ctx context.Context,
	executor *coreinsights.Executor,
	dashboard DashboardWithTiles,
	overrides DashboardQueryOverrides,
) []DashboardTileQueryOutcome {
	now := time.Now()

	type indexedTile struct {
		index int
		tile  dbread.DashboardTile
	}
	insightTiles := make([]indexedTile, 0, len(dashboard.Tiles))
	for index, tile := range dashboard.Tiles {
		if TileKind(tile.Kind) == TileKindInsight {
			insightTiles = append(insightTiles, indexedTile{index: index, tile: tile})
		}
	}
	if len(insightTiles) == 0 {
		return nil
	}

	outcomes := make([]DashboardTileQueryOutcome, len(insightTiles))
	sem := make(chan struct{}, maxConcurrentDashboardTileQueries)
	group, groupCtx := errgroup.WithContext(ctx)

	for outcomeIndex, entry := range insightTiles {
		group.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			outcomes[outcomeIndex] = queryDashboardTile(
				groupCtx,
				executor,
				dashboard.Dashboard.ProjectID,
				entry.tile,
				overrides,
				now,
			)
			return nil
		})
	}
	_ = group.Wait()

	return outcomes
}

func queryDashboardTile(
	ctx context.Context,
	executor *coreinsights.Executor,
	projectID string,
	tile dbread.DashboardTile,
	overrides DashboardQueryOverrides,
	now time.Time,
) DashboardTileQueryOutcome {
	outcome := DashboardTileQueryOutcome{TileID: tile.ID}

	if len(tile.InsightQuery) == 0 {
		return tileQueryClientError(ctx, tile.ID, fmt.Sprintf("tile %s: insight tile row missing query", tile.ID))
	}

	storedQuery, err := MapToQueryMessage(tile.InsightQuery)
	if err != nil {
		return tileQueryClientError(ctx, tile.ID, err.Error())
	}
	if len(storedQuery.GetEvents()) == 0 {
		return tileQueryClientError(ctx, tile.ID, "tile query requires at least one event")
	}

	effectiveQuery, err := buildEffectiveTileQuery(
		storedQuery,
		TileDefaultTimeRangePresetFromDB(TileKindInsight, tile.DefaultTimeRange),
		overrides,
		now,
	)
	if err != nil {
		return tileQueryClientError(ctx, tile.ID, err.Error())
	}

	result, err := coreinsights.ExecuteQuery(ctx, executor, projectID, effectiveQuery)
	if err != nil {
		var invalid *coreinsights.InvalidQueryError
		if errors.As(err, &invalid) {
			outcome.ErrorMessage = "invalid query parameters: " + invalid.Message
			return outcome
		}
		outcome.ErrorMessage = "query failed"
		return outcome
	}

	outcome.Result = result
	return outcome
}

func tileQueryClientError(ctx context.Context, tileID, message string) DashboardTileQueryOutcome {
	slog.WarnContext(ctx, "dashboard tile query skipped",
		slog.String("tile_id", tileID),
		slog.String("reason", message))
	return DashboardTileQueryOutcome{
		TileID:       tileID,
		ErrorMessage: message,
	}
}

func buildEffectiveTileQuery(
	stored *insightsv1.QueryRequest,
	preset commonv1.TimeRangePreset,
	overrides DashboardQueryOverrides,
	now time.Time,
) (*insightsv1.QueryRequest, error) {
	effective := proto.Clone(stored).(*insightsv1.QueryRequest)

	timeRange, err := resolveEffectiveTileTimeRange(stored, preset, overrides, now)
	if err != nil {
		return nil, err
	}
	effective.TimeRange = timeRange

	if overrides.Granularity != insightsv1.Granularity_GRANULARITY_UNSPECIFIED {
		effective.Granularity = overrides.Granularity.Enum()
	} else {
		effective.Granularity = defaultQueryGranularity(stored).Enum()
	}

	return effective, nil
}

func resolveEffectiveTileTimeRange(
	stored *insightsv1.QueryRequest,
	preset commonv1.TimeRangePreset,
	overrides DashboardQueryOverrides,
	now time.Time,
) (*commonv1.TimeRange, error) {
	if overrides.TimeRange != nil {
		if !validAbsoluteTimeRange(overrides.TimeRange) {
			return nil, fmt.Errorf("invalid time range override")
		}
		return overrides.TimeRange, nil
	}
	if validAbsoluteTimeRange(stored.GetTimeRange()) {
		return stored.GetTimeRange(), nil
	}

	timeRange := ResolveDashboardTimeRangePreset(preset, nil, now)
	if !validAbsoluteTimeRange(timeRange) {
		return nil, fmt.Errorf("missing effective time range")
	}
	return timeRange, nil
}

func defaultQueryGranularity(query *insightsv1.QueryRequest) insightsv1.Granularity {
	granularity := query.GetGranularity()
	switch granularity {
	case insightsv1.Granularity_GRANULARITY_HOUR,
		insightsv1.Granularity_GRANULARITY_DAY,
		insightsv1.Granularity_GRANULARITY_WEEK,
		insightsv1.Granularity_GRANULARITY_MONTH:
		return granularity
	default:
		return insightsv1.Granularity_GRANULARITY_DAY
	}
}
