// Package dashboards serves shared.dashboards.v1: a project's dashboards for a
// private key or a sign-in. It can't delete a dashboard or share one.
package dashboards

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	shareddashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1/shareddashboardsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

type Server struct {
	shareddashboardsv1connect.UnimplementedDashboardsServiceHandler
	service *coredashboards.Service
	baseURL string
}

// NewServer links dashboards under dashboardBaseURL, PUG_DASHBOARD_BASE_URL; empty
// leaves them without a link.
func NewServer(service *coredashboards.Service, dashboardBaseURL string) *Server {
	return &Server{service: service, baseURL: strings.TrimRight(dashboardBaseURL, "/")}
}

func (s *Server) List(
	ctx context.Context,
	_ *connect.Request[shareddashboardsv1.ListRequest],
) (*connect.Response[shareddashboardsv1.ListResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}
	dashboards, err := s.service.ListDashboards(ctx, principal.Project.ID)
	if err != nil {
		return nil, internalError()
	}
	out := make([]*shareddashboardsv1.DashboardSummary, len(dashboards))
	for i, d := range dashboards {
		out[i] = &shareddashboardsv1.DashboardSummary{
			Id:          proto.String(d.Dashboard.ID),
			DisplayName: proto.String(d.Dashboard.DisplayName),
			Description: proto.String(d.Dashboard.Description),
			TileCount:   proto.Int32(int32(len(d.Tiles))),
			Shared:      proto.Bool(d.Share != nil),
			UpdateTime:  timestamppb.New(d.Dashboard.UpdateTime.Time),
		}
	}
	return connect.NewResponse(&shareddashboardsv1.ListResponse{Dashboards: out}), nil
}

func (s *Server) Get(
	ctx context.Context,
	req *connect.Request[shareddashboardsv1.GetRequest],
) (*connect.Response[shareddashboardsv1.GetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}
	dashboard, err := s.service.GetDashboard(ctx, principal.Project.ID, req.Msg.GetId())
	if errors.Is(err, coredashboards.ErrDashboardNotFound) {
		return nil, dashboardNotFound(req.Msg.GetId())
	}
	if err != nil {
		return nil, internalError()
	}
	tiles := make([]*shareddashboardsv1.Tile, len(dashboard.Tiles))
	for i, row := range dashboard.Tiles {
		msg, err := coredashboards.TileToRPC(ctx, row)
		if err != nil {
			slog.ErrorContext(ctx, "failed to decode dashboard tile", slogx.Error(err),
				slog.String("dashboard_id", row.DashboardID), slog.String("tile_id", row.ID))
			telemetry.RecordError(ctx, err)
			return nil, internalError()
		}
		tiles[i] = tileToRPC(msg)
	}
	d := dashboard.Dashboard
	return connect.NewResponse(&shareddashboardsv1.GetResponse{Dashboard: &shareddashboardsv1.Dashboard{
		Id:                 proto.String(d.ID),
		DisplayName:        proto.String(d.DisplayName),
		Description:        proto.String(d.Description),
		DefaultTimeRange:   coredashboards.DashboardDefaultTimeRangePresetFromDB(ctx, d.DefaultTimeRange).Enum(),
		DefaultGranularity: coredashboards.DashboardGranularityFromDB(ctx, d.DefaultGranularity).Enum(),
		Shared:             proto.Bool(dashboard.Share != nil),
		UpdateTime:         timestamppb.New(d.UpdateTime.Time),
		Url:                s.link(principal.Project.ID, d.ID),
		Tiles:              tiles,
	}}), nil
}

func (s *Server) Upsert(
	ctx context.Context,
	req *connect.Request[shareddashboardsv1.UpsertRequest],
) (*connect.Response[shareddashboardsv1.UpsertResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	in := coredashboards.PatchDashboardInput{
		ID:                 msg.GetId(),
		DisplayName:        msg.DisplayName,
		Description:        msg.Description,
		DefaultTimeRange:   chosen(msg.DefaultTimeRange),
		DefaultGranularity: chosen(msg.DefaultGranularity),
		Tiles:              make([]coredashboards.PatchTileInput, len(msg.GetTiles())),
		Remove:             msg.GetRemove(),
		Timezone:           principal.Project.ReportingTimezone,
	}
	if msg.GetUpdateTime() != nil {
		in.UpdateTime = msg.GetUpdateTime().AsTime()
	}
	for i, t := range msg.GetTiles() {
		in.Tiles[i] = coredashboards.PatchTileInput{
			ID:          t.GetId(),
			DisplayName: t.DisplayName,
			Description: t.Description,
			Spec:        t.GetSpec(),
			Markdown:    t.Markdown,
			ViewMode:    viewModeFromChart(t.GetChart()),
			Position:    positionToStored(t.GetPosition()),
		}
	}

	res, err := s.service.PatchDashboard(ctx, principal.Project.ID, in)
	if err != nil {
		return nil, upsertError(err, msg.GetId())
	}
	return connect.NewResponse(&shareddashboardsv1.UpsertResponse{
		Id:         proto.String(res.ID),
		UpdateTime: timestamppb.New(res.UpdateTime),
		Url:        s.link(principal.Project.ID, res.ID),
		TileIds:    res.TileIDs,
	}), nil
}

// link opens the dashboard in its own project, which the app may not have active.
func (s *Server) link(projectID, dashboardID string) *string {
	if s.baseURL == "" {
		return nil
	}
	return proto.String(s.baseURL + "/dashboards/" + url.PathEscape(dashboardID) + "?project=" + url.QueryEscape(projectID))
}

func upsertError(err error, dashboardID string) error {
	switch {
	case errors.Is(err, coredashboards.ErrDashboardNotFound):
		return dashboardNotFound(dashboardID)
	case errors.Is(err, coredashboards.ErrDashboardTileNotFound):
		return apperr.NotFound(apperr.ReasonDashboardTileNotFound, "a tile id in tiles or remove isn't on the dashboard")
	case errors.Is(err, coredashboards.ErrDashboardTileDisplayNameConflict):
		return apperr.AlreadyExists(apperr.ReasonDashboardTileNameConflict, "tile display name already in use")
	case errors.Is(err, coredashboards.ErrDashboardChanged):
		return apperr.FailedPrecondition(apperr.ReasonDashboardChanged, "the dashboard changed since you read it; read it again")
	case errors.Is(err, coredashboards.ErrDashboardShared):
		return apperr.FailedPrecondition(apperr.ReasonDashboardShared, "the dashboard has sharing on, so it can only be edited in Pug")
	case errors.Is(err, coredashboards.ErrDashboardTileLimit):
		return apperr.Invalid(apperr.ReasonDashboardTileLimit,
			fmt.Sprintf("a dashboard holds at most %d tiles", coredashboards.MaxDashboardTiles))
	}
	if missing, ok := errors.AsType[*coredashboards.TileNotFoundError](err); ok {
		return apperr.NotFound(apperr.ReasonDashboardTileNotFound, missing.Error(), apperr.Resource("dashboard_tile", missing.ID))
	}
	if position, ok := errors.AsType[*coredashboards.TilePositionError](err); ok {
		return apperr.Invalid(apperr.ReasonInvalidTilePosition, position.Message)
	}
	if query, ok := errors.AsType[*coredashboards.TileQueryError](err); ok {
		return apperr.Invalid(apperr.ReasonInvalidInsightQuery, query.Message)
	}
	if invalid, ok := errors.AsType[*coredashboards.InvalidTileError](err); ok {
		return apperr.Invalid(apperr.ReasonInvalidTileContent, invalid.Message)
	}
	return internalError()
}

func dashboardNotFound(id string) error {
	return apperr.NotFound(apperr.ReasonDashboardNotFound, "dashboard not found", apperr.Resource("dashboard", id))
}

func internalError() error {
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
