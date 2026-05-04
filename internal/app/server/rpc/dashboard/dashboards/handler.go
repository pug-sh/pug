package dashboards

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

type Server struct {
	service *coreprojects.Service
	dashboardsv1connect.UnimplementedDashboardsServiceHandler
}

func NewServer(service *coreprojects.Service) *Server {
	return &Server{service: service}
}

func (s *Server) Create(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceCreateRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceCreateResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	dashboard, err := s.service.CreateDashboard(ctx, principal.Project.ID, req.Msg.GetDisplayName(), req.Msg.GetDescription())
	if err != nil {
		slog.ErrorContext(ctx, "failed to create dashboard", slogx.Error(err), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceCreateResponse{
		Dashboard: wDashboardToRPC(dashboard, nil),
	}), nil
}

func (s *Server) List(
	ctx context.Context,
	_ *connect.Request[dashboardsv1.DashboardsServiceListRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceListResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	dashboards, err := s.service.ListDashboards(ctx, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dashboards", slogx.Error(err), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*dashboardsv1.Dashboard, 0, len(dashboards))
	for _, dashboard := range dashboards {
		msg, err := roDashboardToRPC(dashboard)
		if err != nil {
			slog.ErrorContext(ctx, "failed to encode dashboard", slogx.Error(err), slog.String("dashboard_id", dashboard.Dashboard.ID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		result = append(result, msg)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceListResponse{Dashboards: result}), nil
}

func (s *Server) Get(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceGetRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceGetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	dashboard, err := s.service.GetDashboard(ctx, principal.Project.ID, req.Msg.GetId())
	if err != nil {
		if errors.Is(err, coreprojects.ErrDashboardNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
		}
		slog.ErrorContext(ctx, "failed to get dashboard", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	msg, err := roDashboardToRPC(dashboard)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard", slogx.Error(err), slog.String("dashboard_id", dashboard.Dashboard.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceGetResponse{Dashboard: msg}), nil
}

func (s *Server) UpdateDisplayName(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceUpdateDisplayNameRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceUpdateDisplayNameResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	dashboard, err := s.service.UpdateDashboardDisplayName(ctx, principal.Project.ID, req.Msg.GetId(), req.Msg.GetDisplayName(), req.Msg.GetDescription())
	if err != nil {
		if errors.Is(err, coreprojects.ErrDashboardNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
		}
		slog.ErrorContext(ctx, "failed to update dashboard display name", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpdateDisplayNameResponse{
		Dashboard: wDashboardToRPC(dashboard, nil),
	}), nil
}

func (s *Server) Delete(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceDeleteRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceDeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	if err := s.service.DeleteDashboard(ctx, principal.Project.ID, req.Msg.GetId()); err != nil {
		if errors.Is(err, coreprojects.ErrDashboardNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
		}
		slog.ErrorContext(ctx, "failed to delete dashboard", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceDeleteResponse{}), nil
}

func (s *Server) CreateInsight(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceCreateInsightRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceCreateInsightResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	insight, err := s.service.CreateDashboardInsight(ctx, principal.Project.ID, req.Msg.GetDashboardId(), req.Msg.GetDisplayName(), req.Msg.GetDescription(), req.Msg.GetQuery(), req.Msg.GetLayouts())
	if err != nil {
		if errors.Is(err, coreprojects.ErrDashboardNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
		}
		slog.ErrorContext(ctx, "failed to create dashboard insight", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetDashboardId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	msg, err := wInsightToRPC(insight)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard insight", slogx.Error(err), slog.String("dashboard_id", req.Msg.GetDashboardId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceCreateInsightResponse{Insight: msg}), nil
}

func (s *Server) UpdateInsight(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceUpdateInsightRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceUpdateInsightResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	insight, err := s.service.UpsertDashboardInsight(ctx, principal.Project.ID, req.Msg.GetDashboardId(), req.Msg.GetId(), req.Msg.GetDisplayName(), req.Msg.GetDescription(), req.Msg.GetQuery(), req.Msg.GetLayouts())
	if err != nil {
		if errors.Is(err, coreprojects.ErrDashboardInsightNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard insight not found"))
		}
		slog.ErrorContext(ctx, "failed to update dashboard insight", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetDashboardId()), slog.String("insight_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	msg, err := wInsightToRPC(insight)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard insight", slogx.Error(err), slog.String("insight_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpdateInsightResponse{Insight: msg}), nil
}

func (s *Server) DeleteInsight(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceDeleteInsightRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceDeleteInsightResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	if err := s.service.DeleteDashboardInsight(ctx, principal.Project.ID, req.Msg.GetDashboardId(), req.Msg.GetId()); err != nil {
		if errors.Is(err, coreprojects.ErrDashboardInsightNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard insight not found"))
		}
		slog.ErrorContext(ctx, "failed to delete dashboard insight", slogx.Error(err), slog.String("project_id", principal.Project.ID), slog.String("dashboard_id", req.Msg.GetDashboardId()), slog.String("insight_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceDeleteInsightResponse{}), nil
}
