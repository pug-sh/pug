package insights

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/fivebitsio/cotton/internal/slogx"
)

// connectCtxErr wraps a context error in the appropriate Connect error code.
func connectCtxErr(err error) error {
	code := connect.CodeCanceled
	msg := "request canceled"
	if errors.Is(err, context.DeadlineExceeded) {
		code = connect.CodeDeadlineExceeded
		msg = "request timed out"
	}
	return connect.NewError(code, errors.New(msg))
}

type server struct {
	service  *coreinsights.Service
	executor *coreinsights.Executor
	insightsv1connect.UnimplementedInsightsServiceHandler
}

// NewServer creates a new InsightsService handler.
func NewServer(service *coreinsights.Service, executor *coreinsights.Executor) *server {
	if service == nil {
		panic("insights: service is nil")
	}
	if executor == nil {
		panic("insights: executor is nil")
	}
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
		return nil, connectCtxErr(err)
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
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		series, err := coreinsights.GroupSeries(rows, q.Properties())
		if err != nil {
			slog.ErrorContext(ctx, "failed to group trend series", slogx.Error(err),
				slog.String("projectID", projectID))
			telemetry.RecordError(ctx, err)
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
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Segmentation{
			Segmentation: &insightsv1.SegmentationResult{Total: proto.Float64(value)},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		var funnelRows []coreinsights.FunnelRow
		var funnelProperties []string
		if req.Msg.GetIncludeStepTiming() {
			q, err := coreinsights.BuildFunnelTimingQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel timing query", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
			}
			users, err := s.executor.QueryFunnelUserEvents(ctx, q)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			funnelRows, err = coreinsights.ComputeFunnelTiming(ctx, users, q.Kinds(), q.WindowSec(), q.NumBreakdowns())
			if err != nil {
				telemetry.RecordError(ctx, err)
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			funnelProperties = q.Properties()
		} else {
			q, err := coreinsights.BuildFunnelCountsQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel query", slogx.Error(err),
					slog.String("projectID", projectID))
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid query parameters"))
			}
			funnelRows, err = s.executor.QueryFunnel(ctx, q)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			funnelProperties = q.Properties()
		}
		funnelSeries, err := coreinsights.GroupFunnelSeries(funnelRows, funnelProperties)
		if err != nil {
			slog.ErrorContext(ctx, "failed to group funnel series", slogx.Error(err),
				slog.String("projectID", projectID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Funnel{
			Funnel: &insightsv1.FunnelResult{Series: funnelSeries},
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
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		retentionSeries, err := coreinsights.GroupRetentionSeries(rows, q.Properties())
		if err != nil {
			slog.ErrorContext(ctx, "failed to group retention series", slogx.Error(err),
				slog.String("projectID", projectID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Retention{
			Retention: &insightsv1.RetentionResult{Series: retentionSeries},
		}

	default:
		// Defensive: protovalidate rejects undefined/UNSPECIFIED insight_type values at the interceptor,
		// so this arm is unreachable via the RPC path. A new enum variant added to the proto without a
		// matching case here would land here — log it so operators notice.
		err := errors.New("unsupported insight type")
		slog.WarnContext(ctx, "unsupported insight type reached handler default",
			slogx.Error(err),
			slog.String("projectID", projectID),
			slog.String("insightType", req.Msg.GetInsightType().String()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	return connect.NewResponse(resp), nil
}

// SegmentUsers returns a paginated list of distinct user IDs matching the given filters.
func (s *server) SegmentUsers(
	ctx context.Context,
	req *connect.Request[insightsv1.SegmentUsersRequest],
) (*connect.Response[insightsv1.SegmentUsersResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connectCtxErr(err)
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

	resp := &insightsv1.SegmentUsersResponse{
		DistinctIds: ids,
	}
	if nextPageToken != "" {
		resp.NextPageToken = proto.String(nextPageToken)
	}

	return connect.NewResponse(resp), nil
}

func (s *server) GetFilterSchema(
	ctx context.Context,
	req *connect.Request[insightsv1.GetFilterSchemaRequest],
) (*connect.Response[insightsv1.GetFilterSchemaResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connectCtxErr(err)
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
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(schema), nil
}

func (s *server) GetPropertyValues(
	ctx context.Context,
	req *connect.Request[insightsv1.GetPropertyValuesRequest],
) (*connect.Response[insightsv1.GetPropertyValuesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	values, err := s.service.GetPropertyValues(ctx, projectID, req.Msg.GetPropertyKey(), req.Msg.GetEventKind(), req.Msg.GetSource())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get property values", slogx.Error(err),
			slog.String("projectID", projectID),
			slog.String("propertyKey", req.Msg.GetPropertyKey()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&insightsv1.GetPropertyValuesResponse{Values: values}), nil
}
