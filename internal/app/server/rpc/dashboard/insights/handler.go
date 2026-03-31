package insights

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/dashboard/insights/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/dashboard/insights/v1/insightsv1connect"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type server struct {
	service  *coreinsights.Service
	executor *coreinsights.Executor
	insightsv1connect.UnimplementedInsightsServiceHandler
}

// NewServer creates a new InsightsService handler.
func NewServer(service *coreinsights.Service, executor *coreinsights.Executor) *server {
	return &server{
		service:  service,
		executor: executor,
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
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
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
			errors.New("unsupported insight type"))
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
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
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

func (s *server) GetFilterSchema(
	ctx context.Context,
	req *connect.Request[insightsv1.GetFilterSchemaRequest],
) (*connect.Response[insightsv1.GetFilterSchemaResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	schema, err := s.service.GetFilterSchema(ctx, projectID, req.Msg.GetEventKind())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get filter schema", slogx.Error(err),
			slog.String("projectID", projectID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(schema), nil
}

func (s *server) GetPropertyValues(
	ctx context.Context,
	req *connect.Request[insightsv1.GetPropertyValuesRequest],
) (*connect.Response[insightsv1.GetPropertyValuesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	values, err := s.service.GetPropertyValues(ctx, projectID, req.Msg.PropertyKey, req.Msg.EventKind, req.Msg.Source)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get property values", slogx.Error(err),
			slog.String("projectID", projectID),
			slog.String("propertyKey", req.Msg.PropertyKey))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&insightsv1.GetPropertyValuesResponse{Values: values}), nil
}
