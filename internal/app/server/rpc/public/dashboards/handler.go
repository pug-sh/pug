package dashboards

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	publicdashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1/publicdashboardsv1connect"
)

type Server struct {
	service  *coredashboards.Service
	executor *coreinsights.Executor
	publicdashboardsv1connect.UnimplementedSharedDashboardsServiceHandler
}

func NewServer(service *coredashboards.Service, executor *coreinsights.Executor) *Server {
	if service == nil {
		panic("public dashboards: service is nil")
	}
	if executor == nil {
		panic("public dashboards: executor is nil")
	}
	return &Server{
		service:  service,
		executor: executor,
	}
}

func (s *Server) Query(
	ctx context.Context,
	req *connect.Request[publicdashboardsv1.SharedDashboardsServiceQueryRequest],
) (*connect.Response[publicdashboardsv1.SharedDashboardsServiceQueryResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	dashboard, err := s.service.GetSharedDashboard(ctx, req.Msg.GetShareId())
	if err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard_share", req.Msg.GetShareId()))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	overrides := coredashboards.DashboardQueryOverrides{
		TimeRange:   req.Msg.GetTimeRange(),
		Granularity: req.Msg.GetGranularity(),
	}
	rendered, err := coredashboards.RenderDashboard(ctx, s.executor, dashboard, overrides)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, rpc.ConnectCtxErr(err)
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&publicdashboardsv1.SharedDashboardsServiceQueryResponse{
		Dashboard: coredashboards.RenderedDashboardToRPC(ctx, rendered),
	}), nil
}
