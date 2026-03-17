package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/insights/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/insights/v1/insightsv1connect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type server struct {
	executor *coreinsights.Executor
	insightsv1connect.UnimplementedInsightsServiceHandler
}

// NewServer creates a new InsightsService handler backed by the given ClickHouse connection.
func NewServer(ch driver.Conn) *server {
	return &server{
		executor: coreinsights.NewExecutor(ch),
	}
}

// Query handles analytics queries for trends and segmentation insight types.
func (s *server) Query(
	ctx context.Context,
	req *connect.Request[insightsv1.QueryRequest],
) (*connect.Response[insightsv1.QueryResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Validate time range
	tr := req.Msg.GetTimeRange()
	if tr == nil || tr.GetFrom() == nil || tr.GetTo() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("time_range with from and to is required"))
	}
	if !tr.GetFrom().AsTime().Before(tr.GetTo().AsTime()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("time_range.from must be before time_range.to"))
	}

	// Validate insight type
	insightType := req.Msg.GetInsightType()
	if insightType != insightsv1.InsightType_INSIGHT_TYPE_TRENDS &&
		insightType != insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("insight_type must be TRENDS or SEGMENTATION"))
	}

	// Validate upper bounds
	if req.Msg.GetBreakdownLimit() < 0 || req.Msg.GetBreakdownLimit() > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("breakdown_limit must be between 0 and 100"))
	}

	sql, args, err := coreinsights.BuildQuery(req.Msg, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build insights query", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	var series []*insightsv1.Series

	switch insightType {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		breakdowns := req.Msg.GetBreakdowns()
		if len(breakdowns) == 0 {
			// Simple trends: one series of (time, value) points
			rows, err := s.executor.QueryTrends(ctx, sql, args)
			if err != nil {
				slog.ErrorContext(ctx, "failed to query trends", slogx.Error(err))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			points := make([]*insightsv1.DataPoint, 0, len(rows))
			for _, r := range rows {
				points = append(points, &insightsv1.DataPoint{
					Time:  timestamppb.New(r.Time),
					Value: r.Value,
				})
			}
			eventKind := ""
			if len(req.Msg.GetEvents()) > 0 {
				eventKind = req.Msg.GetEvents()[0].GetKind()
			}
			series = []*insightsv1.Series{
				{
					EventKind: eventKind,
					Points:    points,
				},
			}
		} else {
			// Breakdown trends: group rows into Series by their breakdown tuple
			rows, err := s.executor.QueryTrendsWithBreakdowns(ctx, sql, args, len(breakdowns))
			if err != nil {
				slog.ErrorContext(ctx, "failed to query trends with breakdowns", slogx.Error(err))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}

			// Group rows by breakdown values (stringified key)
			type seriesEntry struct {
				breakdown map[string]string
				points    []*insightsv1.DataPoint
			}
			// Use a slice to preserve insertion order
			var orderedKeys []string
			entriesByKey := map[string]*seriesEntry{}

			for _, r := range rows {
				// Build a canonical key from the breakdown values
				key := buildBreakdownKey(r.Breakdowns)
				if _, ok := entriesByKey[key]; !ok {
					orderedKeys = append(orderedKeys, key)
					bd := make(map[string]string, len(breakdowns))
					for i, bk := range breakdowns {
						bd[bk.GetProperty()] = r.Breakdowns[i]
					}
					entriesByKey[key] = &seriesEntry{breakdown: bd}
				}
				entriesByKey[key].points = append(entriesByKey[key].points, &insightsv1.DataPoint{
					Time:  timestamppb.New(r.Time),
					Value: r.Value,
				})
			}

			series = make([]*insightsv1.Series, 0, len(orderedKeys))
			for _, k := range orderedKeys {
				e := entriesByKey[k]
				series = append(series, &insightsv1.Series{
					Breakdown: e.breakdown,
					Points:    e.points,
				})
			}
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		value, err := s.executor.QueryScalar(ctx, sql, args)
		if err != nil {
			slog.ErrorContext(ctx, "failed to query segmentation", slogx.Error(err))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		series = []*insightsv1.Series{
			{
				Total: value,
			},
		}
	}

	return connect.NewResponse(&insightsv1.QueryResponse{Series: series}), nil
}

// SegmentUsers returns a paginated list of distinct user IDs matching the given filters.
func (s *server) SegmentUsers(
	ctx context.Context,
	req *connect.Request[insightsv1.SegmentUsersRequest],
) (*connect.Response[insightsv1.SegmentUsersResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Validate time range
	tr := req.Msg.GetTimeRange()
	if tr == nil || tr.GetFrom() == nil || tr.GetTo() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("time_range with from and to is required"))
	}
	if !tr.GetFrom().AsTime().Before(tr.GetTo().AsTime()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("time_range.from must be before time_range.to"))
	}

	// Validate page_size upper bound
	if req.Msg.GetPageSize() > 1000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must be <= 1000"))
	}

	sql, args, err := coreinsights.BuildSegmentUsersQuery(req.Msg, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build segment users query", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ids, err := s.executor.QueryDistinctIDs(ctx, sql, args)
	if err != nil {
		slog.ErrorContext(ctx, "failed to query distinct IDs", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	pageSize := req.Msg.GetPageSize()
	if pageSize == 0 {
		pageSize = 100
	}

	// Build next page token: set to last ID when page is full
	var nextPageToken string
	if int32(len(ids)) == pageSize {
		nextPageToken = ids[len(ids)-1]
	}

	return connect.NewResponse(&insightsv1.SegmentUsersResponse{
		DistinctIds:   ids,
		NextPageToken: nextPageToken,
	}), nil
}

// buildBreakdownKey creates a canonical string key from a slice of breakdown values.
// Uses fmt.Sprintf("%q") to encode values unambiguously, avoiding collisions.
func buildBreakdownKey(breakdowns []string) string {
	return fmt.Sprintf("%q", breakdowns)
}
