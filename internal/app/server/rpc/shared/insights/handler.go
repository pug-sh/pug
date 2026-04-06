package insights

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1/insightsv1connect"
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

// Query handles analytics queries for trends, segmentation, funnel, and retention insight types.
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

	projectID := principal.Project.ID

	resp := &insightsv1.QueryResponse{}

	switch req.Msg.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, err := coreinsights.BuildTrendsQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build trends query", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
		}
		rows, err := s.executor.QueryTrends(ctx, q)
		if err != nil {
			slog.ErrorContext(ctx, "failed to query trends", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		series, err := coreinsights.GroupSeries(rows, q.Properties())
		if err != nil {
			slog.ErrorContext(ctx, "failed to group trend series", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Trends{
			Trends: &insightsv1.TrendsResult{Series: series},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		q, err := coreinsights.BuildSegmentationQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build segmentation query", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
		}
		value, err := s.executor.QueryScalar(ctx, q)
		if err != nil {
			slog.ErrorContext(ctx, "failed to query segmentation", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Segmentation{
			Segmentation: &insightsv1.SegmentationResult{Total: value},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		var funnelRows []coreinsights.FunnelRow
		if req.Msg.GetIncludeStepTiming() {
			q, err := coreinsights.BuildFunnelTimingQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel timing query", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
			}
			users, err := s.executor.QueryFunnelUserEvents(ctx, q)
			if err != nil {
				slog.ErrorContext(ctx, "failed to query funnel user events", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			funnelRows, err = coreinsights.ComputeFunnelTiming(users, q.Kinds(), q.WindowSec())
			if err != nil {
				slog.ErrorContext(ctx, "failed to compute funnel timing", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
		} else {
			q, err := coreinsights.BuildFunnelCountsQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel query", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
			}
			funnelRows, err = s.executor.QueryFunnel(ctx, q)
			if err != nil {
				slog.ErrorContext(ctx, "failed to query funnel", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
		}
		steps := make([]*insightsv1.FunnelStep, 0, len(funnelRows))
		for _, row := range funnelRows {
			steps = append(steps, &insightsv1.FunnelStep{
				EventKind:               row.EventKind,
				Total:                   row.Value,
				AvgTimeToConvertSeconds: row.AvgConvertSeconds,
			})
		}
		resp.Result = &insightsv1.QueryResponse_Funnel{
			Funnel: &insightsv1.FunnelResult{Steps: steps},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := coreinsights.BuildRetentionQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build retention query", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
		}
		rows, err := s.executor.QueryRetention(ctx, q)
		if err != nil {
			slog.ErrorContext(ctx, "failed to query retention", slogx.Error(err),
				slog.String("projectID", projectID))
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Retention{
			Retention: &insightsv1.RetentionResult{Cohorts: coreinsights.GroupRetentionCohorts(rows)},
		}

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("unsupported insight type"))
	}

	return connect.NewResponse(resp), nil
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

	ids, err := s.executor.QueryStringColumn(ctx, sql, args)
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
