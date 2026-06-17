package insights

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
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

	// Bucketing timezone is server-authoritative: inject the project's stored
	// reporting_timezone, ignoring any client-supplied value, so day/week/month
	// buckets align to the project's calendar consistently for every viewer.
	req.Msg.Timezone = &principal.Project.ReportingTimezone

	resp, err := coreinsights.ExecuteQuery(ctx, s.executor, principal.Project.ID, req.Msg, time.Now())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, rpc.ConnectCtxErr(err)
		}
		var invalid *coreinsights.InvalidQueryError
		if errors.As(err, &invalid) {
			return nil, apperr.Invalid(apperr.ReasonInvalidInsightQuery, "invalid query parameters: "+invalid.Message)
		}
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
