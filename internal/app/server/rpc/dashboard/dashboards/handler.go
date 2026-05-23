package dashboards

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

type Server struct {
	service  *coredashboards.Service
	executor *coreinsights.Executor
	dashboardsv1connect.UnimplementedDashboardsServiceHandler
}

func NewServer(service *coredashboards.Service, executor *coreinsights.Executor) *Server {
	if service == nil {
		panic("dashboards: service is nil")
	}
	if executor == nil {
		panic("dashboards: executor is nil")
	}
	return &Server{
		service:  service,
		executor: executor,
	}
}

// serviceErrToConnect maps a non-sentinel service error to a connect error.
// Context cancellation / deadline arriving mid-request surface as wrapped
// pgx errors here, so the catch-all branch checks errors.Is before falling
// back to CodeInternal. Service layer has already logged + recorded the
// non-context error path; we don't duplicate.
func serviceErrToConnect(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return rpc.ConnectCtxErr(err)
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func (s *Server) Create(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceCreateRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceCreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboard, err := s.service.CreateDashboard(ctx, principal.Project.ID, req.Msg.GetDisplayName(), req.Msg.GetDescription())
	if err != nil {
		return nil, serviceErrToConnect(err)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceCreateResponse{
		Dashboard: wDashboardToRPC(dashboard),
	}), nil
}

func (s *Server) List(
	ctx context.Context,
	_ *connect.Request[dashboardsv1.DashboardsServiceListRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceListResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboards, err := s.service.ListDashboards(ctx, principal.Project.ID)
	if err != nil {
		return nil, serviceErrToConnect(err)
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
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboard, err := s.service.GetDashboard(ctx, principal.Project.ID, req.Msg.GetId())
	if err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetId()))
		}
		return nil, serviceErrToConnect(err)
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
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboard, err := s.service.UpdateDashboardDisplayName(ctx, principal.Project.ID, req.Msg.GetId(), req.Msg.GetDisplayName(), req.Msg.GetDescription())
	if err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetId()))
		}
		return nil, serviceErrToConnect(err)
	}

	msg, err := roDashboardToRPC(dashboard)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard", slogx.Error(err), slog.String("dashboard_id", dashboard.Dashboard.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpdateDisplayNameResponse{Dashboard: msg}), nil
}

func (s *Server) Delete(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceDeleteRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceDeleteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.DeleteDashboard(ctx, principal.Project.ID, req.Msg.GetId()); err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetId()))
		}
		return nil, serviceErrToConnect(err)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceDeleteResponse{}), nil
}

func (s *Server) CreateTile(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceCreateTileRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceCreateTileResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	content, err := tileContentFromCreateRPC(req.Msg.GetContent())
	if err != nil {
		slog.WarnContext(ctx, "invalid tile content", slogx.Error(err), slog.String("dashboard_id", req.Msg.GetDashboardId()))
		return nil, apperr.Invalid(apperr.ReasonInvalidTileContent, "tile content required")
	}

	tile, err := s.service.CreateDashboardTile(
		ctx,
		principal.Project.ID,
		req.Msg.GetDashboardId(),
		req.Msg.GetDisplayName(),
		req.Msg.GetDescription(),
		content,
		req.Msg.GetViewMode(),
		req.Msg.GetDefaultTimeRange(),
		req.Msg.GetLayouts(),
	)
	if err != nil {
		switch {
		case errors.Is(err, coredashboards.ErrDashboardNotFound):
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetDashboardId()))
		case errors.Is(err, coredashboards.ErrDashboardTileDisplayNameConflict):
			return nil, apperr.AlreadyExists(apperr.ReasonDashboardTileNameConflict, "tile display name already in use")
		}
		// Service already logged + recorded; do not duplicate.
		return nil, serviceErrToConnect(err)
	}

	msg, err := wTileToRPC(tile)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard tile",
			slogx.Error(err),
			slog.String("dashboard_id", req.Msg.GetDashboardId()),
			slog.String("tile_id", tile.ID),
		)
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceCreateTileResponse{Tile: msg}), nil
}

func (s *Server) UpdateTile(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceUpdateTileRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceUpdateTileResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	content, err := tileContentFromUpdateRPC(req.Msg.GetContent())
	if err != nil {
		slog.WarnContext(ctx, "invalid tile content", slogx.Error(err), slog.String("tile_id", req.Msg.GetId()))
		return nil, apperr.Invalid(apperr.ReasonInvalidTileContent, "tile content required")
	}

	tile, err := s.service.UpdateDashboardTile(
		ctx,
		principal.Project.ID,
		req.Msg.GetDashboardId(),
		req.Msg.GetId(),
		req.Msg.GetDisplayName(),
		req.Msg.GetDescription(),
		content,
		req.Msg.GetViewMode(),
		req.Msg.GetDefaultTimeRange(),
		req.Msg.GetLayouts(),
	)
	if err != nil {
		switch {
		case errors.Is(err, coredashboards.ErrDashboardTileNotFound):
			return nil, apperr.NotFound(apperr.ReasonDashboardTileNotFound, "dashboard tile not found", apperr.Resource("dashboard_tile", req.Msg.GetId()))
		case errors.Is(err, coredashboards.ErrDashboardTileDisplayNameConflict):
			return nil, apperr.AlreadyExists(apperr.ReasonDashboardTileNameConflict, "tile display name already in use")
		}
		// Service already logged + recorded; do not duplicate.
		return nil, serviceErrToConnect(err)
	}

	msg, err := wTileToRPC(tile)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard tile",
			slogx.Error(err),
			slog.String("dashboard_id", req.Msg.GetDashboardId()),
			slog.String("tile_id", req.Msg.GetId()),
		)
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpdateTileResponse{Tile: msg}), nil
}

func (s *Server) DeleteTile(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceDeleteTileRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceDeleteTileResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.DeleteDashboardTile(ctx, principal.Project.ID, req.Msg.GetDashboardId(), req.Msg.GetId()); err != nil {
		if errors.Is(err, coredashboards.ErrDashboardTileNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardTileNotFound, "dashboard tile not found", apperr.Resource("dashboard_tile", req.Msg.GetId()))
		}
		return nil, serviceErrToConnect(err)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceDeleteTileResponse{}), nil
}

func (s *Server) QueryDashboard(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceQueryDashboardRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceQueryDashboardResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboard, err := s.service.GetDashboard(ctx, principal.Project.ID, req.Msg.GetDashboardId())
	if err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetDashboardId()))
		}
		return nil, serviceErrToConnect(err)
	}

	overrides := coredashboards.DashboardQueryOverrides{
		TimeRange:   req.Msg.GetTimeRangeOverride(),
		Granularity: req.Msg.GetGranularityOverride(),
	}
	outcomes := coredashboards.QueryDashboardTiles(ctx, s.executor, dashboard, overrides)

	results := make([]*dashboardsv1.DashboardTileQueryResult, 0, len(outcomes))
	for _, outcome := range outcomes {
		msg := &dashboardsv1.DashboardTileQueryResult{
			TileId: proto.String(outcome.TileID),
		}
		if outcome.ErrorMessage != "" {
			msg.Outcome = &dashboardsv1.DashboardTileQueryResult_ErrorMessage{
				ErrorMessage: outcome.ErrorMessage,
			}
		} else {
			msg.Outcome = &dashboardsv1.DashboardTileQueryResult_Result{
				Result: outcome.Result,
			}
		}
		results = append(results, msg)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceQueryDashboardResponse{
		Results: results,
	}), nil
}
