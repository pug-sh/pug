package projects

import (
	"context"
	"errors"
	"fmt"
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
func QueryDashboardTiles(
	ctx context.Context,
	executor *coreinsights.Executor,
	dashboard DashboardWithTiles,
	overrides DashboardQueryOverrides,
) []DashboardTileQueryOutcome {
	now := time.Now()
	insightTiles := make([]dbread.DashboardTile, 0, len(dashboard.Tiles))
	for _, tile := range dashboard.Tiles {
		if TileKind(tile.Kind) == TileKindInsight {
			insightTiles = append(insightTiles, tile)
		}
	}
	if len(insightTiles) == 0 {
		return nil
	}

	outcomes := make([]DashboardTileQueryOutcome, len(insightTiles))
	sem := make(chan struct{}, maxConcurrentDashboardTileQueries)
	group, groupCtx := errgroup.WithContext(ctx)

	for index, tile := range insightTiles {
		group.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			outcomes[index] = queryDashboardTile(groupCtx, executor, dashboard.Dashboard.ProjectID, tile, overrides, now)
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
		outcome.ErrorMessage = fmt.Sprintf("tile %s: insight tile row missing query", tile.ID)
		return outcome
	}

	storedQuery, err := MapToQueryMessage(tile.InsightQuery)
	if err != nil {
		outcome.ErrorMessage = err.Error()
		return outcome
	}
	if len(storedQuery.GetEvents()) == 0 {
		outcome.ErrorMessage = "tile query requires at least one event"
		return outcome
	}

	effectiveQuery, err := buildEffectiveTileQuery(
		storedQuery,
		TileDefaultTimeRangePresetFromDB(TileKind(tile.Kind), tile.DefaultTimeRange),
		overrides,
		now,
	)
	if err != nil {
		outcome.ErrorMessage = err.Error()
		return outcome
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

func buildEffectiveTileQuery(
	stored *insightsv1.QueryRequest,
	preset commonv1.TimeRangePreset,
	overrides DashboardQueryOverrides,
	now time.Time,
) (*insightsv1.QueryRequest, error) {
	effective := proto.Clone(stored).(*insightsv1.QueryRequest)

	var timeRange *commonv1.TimeRange
	if overrides.TimeRange != nil {
		timeRange = overrides.TimeRange
	} else {
		timeRange = ResolveDashboardTimeRangePreset(preset, stored.GetTimeRange(), now)
	}
	if timeRange == nil || timeRange.GetFrom() == nil || timeRange.GetTo() == nil {
		return nil, fmt.Errorf("missing effective time range")
	}
	effective.TimeRange = timeRange

	if overrides.Granularity != insightsv1.Granularity_GRANULARITY_UNSPECIFIED {
		effective.Granularity = overrides.Granularity.Enum()
	} else {
		effective.Granularity = defaultQueryGranularity(stored).Enum()
	}

	return effective, nil
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
