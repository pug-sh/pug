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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	insightType := req.Msg.GetInsightType()

	sql, args, err := coreinsights.BuildQuery(req.Msg, principal.Project.ID)
	if err != nil {
		slog.WarnContext(ctx, "failed to build insights query", slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("insightType", insightType.String()))
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
				slog.ErrorContext(ctx, "failed to query trends", slogx.Error(err),
					slog.String("projectID", principal.Project.ID))
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
				slog.ErrorContext(ctx, "failed to query trends with breakdowns", slogx.Error(err),
					slog.String("projectID", principal.Project.ID))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}

			properties := make([]string, len(breakdowns))
			for i, bk := range breakdowns {
				properties[i] = bk.GetProperty()
			}
			series = coreinsights.GroupBreakdownSeries(rows, properties)
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		value, err := s.executor.QueryScalar(ctx, sql, args)
		if err != nil {
			slog.ErrorContext(ctx, "failed to query segmentation", slogx.Error(err),
				slog.String("projectID", principal.Project.ID))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		series = []*insightsv1.Series{
			{
				Total: value,
			},
		}

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported insight type: %v", insightType))
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	sql, args, err := coreinsights.BuildSegmentUsersQuery(req.Msg, principal.Project.ID)
	if err != nil {
		slog.WarnContext(ctx, "failed to build segment users query", slogx.Error(err),
			slog.String("projectID", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ids, err := s.executor.QueryDistinctIDs(ctx, sql, args)
	if err != nil {
		slog.ErrorContext(ctx, "failed to query distinct IDs", slogx.Error(err),
			slog.String("projectID", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	pageSize := req.Msg.GetPageSize()
	if pageSize == 0 {
		pageSize = coreinsights.DefaultPageSize
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
