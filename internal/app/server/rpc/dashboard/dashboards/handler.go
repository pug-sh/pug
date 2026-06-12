package dashboards

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
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

	dashboard, err := s.service.CreateDashboard(ctx, principal.Project.ID, req.Msg.GetDisplayName(), req.Msg.GetDescription(), req.Msg.GetDefaultTimeRange(), req.Msg.GetDefaultGranularity())
	if err != nil {
		return nil, serviceErrToConnect(err)
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceCreateResponse{
		Dashboard: wDashboardToRPC(ctx, dashboard),
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
		msg, err := roDashboardToRPC(ctx, dashboard)
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

	msg, err := roDashboardToRPC(ctx, dashboard)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard", slogx.Error(err), slog.String("dashboard_id", dashboard.Dashboard.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceGetResponse{Dashboard: msg}), nil
}

func (s *Server) Update(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceUpdateRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceUpdateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dashboard, err := s.service.UpdateDashboard(ctx, principal.Project.ID, req.Msg.GetId(), coredashboards.UpdateDashboardInput{
		DisplayName:        req.Msg.GetDisplayName(),
		Description:        req.Msg.GetDescription(),
		DefaultTimeRange:   req.Msg.GetDefaultTimeRange(),
		DefaultGranularity: req.Msg.GetDefaultGranularity(),
		IsPublic:           req.Msg.IsPublic,
	})
	if err != nil {
		if errors.Is(err, coredashboards.ErrDashboardNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetId()))
		}
		return nil, serviceErrToConnect(err)
	}

	msg, err := roDashboardToRPC(ctx, dashboard)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard", slogx.Error(err), slog.String("dashboard_id", dashboard.Dashboard.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpdateResponse{Dashboard: msg}), nil
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

func (s *Server) Upsert(
	ctx context.Context,
	req *connect.Request[dashboardsv1.DashboardsServiceUpsertRequest],
) (*connect.Response[dashboardsv1.DashboardsServiceUpsertResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	tiles := make([]coredashboards.UpsertTileInput, 0, len(req.Msg.GetTiles()))
	for i, t := range req.Msg.GetTiles() {
		converted, err := upsertTileInputFromRPC(t)
		if err != nil {
			// protovalidate has already enforced oneof.required on the input;
			// reaching this branch means a proto change added a new content
			// kind without updating upsertTileInputFromRPC (schema drift), not
			// a client input bug. Map to CodeInternal so the alarm fires.
			slog.ErrorContext(ctx, "schema drift: unrecognized tile content in upsert",
				slogx.Error(err),
				slog.String("dashboard_id", req.Msg.GetId()),
				slog.Int("tile_index", i),
			)
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		tiles = append(tiles, converted)
	}

	dashboard, err := s.service.UpsertDashboard(ctx, principal.Project.ID, req.Msg.GetId(), coredashboards.UpsertDashboardInput{
		DisplayName:        req.Msg.GetDisplayName(),
		Description:        req.Msg.GetDescription(),
		DefaultTimeRange:   req.Msg.GetDefaultTimeRange(),
		DefaultGranularity: req.Msg.GetDefaultGranularity(),
		Tiles:              tiles,
	})
	if err != nil {
		switch {
		case errors.Is(err, coredashboards.ErrDashboardNotFound):
			return nil, apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", req.Msg.GetId()))
		case errors.Is(err, coredashboards.ErrDashboardTileNotFound):
			return nil, apperr.NotFound(apperr.ReasonDashboardTileNotFound, "dashboard tile not found", apperr.Resource("dashboard", req.Msg.GetId()))
		case errors.Is(err, coredashboards.ErrDashboardTileDisplayNameConflict):
			return nil, apperr.AlreadyExists(apperr.ReasonDashboardTileNameConflict, "tile display name already in use")
		case errors.Is(err, coredashboards.ErrDuplicateUpsertTileID):
			return nil, apperr.Invalid(apperr.ReasonInvalidTileContent, "duplicate tile id in request")
		}
		return nil, serviceErrToConnect(err)
	}

	msg, err := roDashboardToRPC(ctx, dashboard)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode upserted dashboard",
			slogx.Error(err),
			slog.String("dashboard_id", req.Msg.GetId()),
		)
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&dashboardsv1.DashboardsServiceUpsertResponse{Dashboard: msg}), nil
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
		TimeRange:   req.Msg.GetTimeRange(),
		Granularity: req.Msg.GetGranularity(),
	}
	rendered, err := coredashboards.RenderDashboard(ctx, s.executor, dashboard, overrides)
	if err != nil {
		// Only a request-level context cancellation/deadline reaches here; per-tile
		// failures are carried in each tile's outcome. Already recorded at source.
		return nil, serviceErrToConnect(err)
	}

	// RenderedDashboardToRPC degrades any undecodable tile to a per-tile error
	// outcome (recorded at source) rather than failing the whole response.
	return connect.NewResponse(&dashboardsv1.DashboardsServiceQueryDashboardResponse{
		Dashboard: coredashboards.RenderedDashboardToRPC(ctx, rendered),
	}), nil
}
