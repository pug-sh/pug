package insights

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

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
		return nil, rpc.ConnectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	projectID := principal.Project.ID

	resp := &insightsv1.QueryResponse{}

	switch req.Msg.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, err := coreinsights.BuildTrendsQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build trends query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
		}
		rows, err := s.executor.QueryTrends(ctx, projectID, q)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		series, err := coreinsights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Trends{
			Trends: &insightsv1.TrendsResult{Series: series},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		q, err := coreinsights.BuildSegmentationQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build segmentation query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
		}
		value, err := s.executor.QueryScalar(ctx, projectID, q)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Segmentation{
			Segmentation: &insightsv1.SegmentationResult{Total: proto.Float64(value)},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		var funnelRows []coreinsights.FunnelRow
		var funnelProperties []string
		var funnelBreakdownLimit int
		if req.Msg.GetIncludeStepTiming() {
			// Run windowFunnel counts and per-user timing in parallel:
			// counts are fast (single-pass windowFunnel), timing is heavier
			// (pre-filtered groupArray). Merging takes counts from windowFunnel
			// and timing stats from ComputeFunnelTiming.
			countsQ, err := coreinsights.BuildFunnelCountsQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel counts query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
			}
			timingQ, err := coreinsights.BuildFunnelTimingQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel timing query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
			}

			var countRows []coreinsights.FunnelRow
			var timingRows []coreinsights.FunnelRow
			eg, egCtx := errgroup.WithContext(ctx)

			eg.Go(func() error {
				rows, err := s.executor.QueryFunnel(egCtx, projectID, countsQ)
				if err != nil {
					return err
				}
				countRows = rows
				return nil
			})

			eg.Go(func() error {
				users, err := s.executor.QueryFunnelUserEvents(egCtx, projectID, timingQ)
				if err != nil {
					return err
				}
				rows, err := coreinsights.ComputeFunnelTiming(egCtx, projectID, users, timingQ.Kinds(), timingQ.WindowSec(), timingQ.NumBreakdowns())
				if err != nil {
					return err
				}
				timingRows = rows
				return nil
			})

			if err := eg.Wait(); err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}

			funnelRows = coreinsights.MergeFunnelCountsAndTiming(countRows, timingRows)
			funnelProperties = countsQ.Properties()
			funnelBreakdownLimit = countsQ.BreakdownLimit()
		} else {
			q, err := coreinsights.BuildFunnelCountsQuery(req.Msg, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
			}
			funnelRows, err = s.executor.QueryFunnel(ctx, projectID, q)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			funnelProperties = q.Properties()
			funnelBreakdownLimit = q.BreakdownLimit()
		}
		funnelSeries, err := coreinsights.GroupFunnelSeries(ctx, funnelRows, funnelProperties, funnelBreakdownLimit)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Funnel{
			Funnel: &insightsv1.FunnelResult{Series: funnelSeries},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := coreinsights.BuildRetentionQuery(req.Msg, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build retention query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
		}
		rows, err := s.executor.QueryRetention(ctx, projectID, q)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		retentionSeries, err := coreinsights.GroupRetentionSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Result = &insightsv1.QueryResponse_Retention{
			Retention: &insightsv1.RetentionResult{Series: retentionSeries},
		}

	default:
		// Defensive: protovalidate rejects undefined/UNSPECIFIED insight_type values at the interceptor,
		// so this arm is unreachable via the RPC path. A new enum variant added to the proto without a
		// matching case here would land here — that is a server-side bug (proto/Go drift), not client
		// input, so log + record at source and return CodeInternal.
		err := errors.New("unsupported insight type")
		slog.ErrorContext(ctx, "unsupported insight type reached handler default",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("insight_type", req.Msg.GetInsightType().String()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(resp), nil
}

// SegmentUsers returns a paginated list of distinct user IDs matching the given filters.
func (s *server) SegmentUsers(
	ctx context.Context,
	req *connect.Request[insightsv1.SegmentUsersRequest],
) (*connect.Response[insightsv1.SegmentUsersResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	sql, args, err := coreinsights.BuildSegmentUsersQuery(req.Msg, principal.Project.ID)
	if err != nil {
		slog.WarnContext(ctx, "failed to build segment users query", slogx.Error(err),
			slog.String("project_id", principal.Project.ID))
		return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+err.Error())
	}

	ids, err := s.executor.QueryStringColumn(ctx, principal.Project.ID, sql, args)
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
	req *connect.Request[commonv1.GetFilterSchemaRequest],
) (*connect.Response[commonv1.GetFilterSchemaResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	projectID := principal.Project.ID

	schema, err := s.service.GetFilterSchema(ctx, projectID, req.Msg.GetEventKind(), req.Msg.GetAllowedTypes())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(schema), nil
}

func (s *server) GetPropertyValues(
	ctx context.Context,
	req *connect.Request[insightsv1.GetPropertyValuesRequest],
) (*connect.Response[insightsv1.GetPropertyValuesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	projectID := principal.Project.ID

	values, err := s.service.GetPropertyValues(ctx, projectID, req.Msg.GetPropertyKey(), req.Msg.GetEventKind(), req.Msg.GetSource())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&insightsv1.GetPropertyValuesResponse{Values: values}), nil
}
