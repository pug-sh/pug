package projects

import (
	"context"
	"log/slog"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/projects"
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/rpc/interceptors"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

type server struct {
	service *projects.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *server {
	service := projects.NewService(pgRO, pgW)

	return &server{
		service: service,
	}
}

// Get returns a project by ID.
func (s *server) Get(
	ctx context.Context,
	req *connect.Request[projectsv1.GetRequest],
) (*connect.Response[projectsv1.GetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if project.ID != req.Msg.Id {
		return nil, connect.NewError(connect.CodePermissionDenied, authn.Errorf("project ID does not match authenticated project"))
	}

	projectData, err := s.service.GetProjectById(ctx, req.Msg.Id)
	if err != nil {
		slog.ErrorContext(ctx, "failed reading from db", slog.Any("error", err), slog.String("projectId", project.ID), slog.String("id", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.GetResponse{Project: roToRPCMsg(projectData)}), nil
}

// BatchGet returns all projects for the authenticated customer.
func (s *server) BatchGet(
	ctx context.Context,
	_ *connect.Request[projectsv1.BatchGetRequest],
) (*connect.Response[projectsv1.BatchGetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	projectsData, err := s.service.GetProjectsByCustomerId(ctx, project.CustomerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed reading from db", slog.Any("error", err), slog.String("customerId", project.CustomerID))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	projects := make([]*projectsv1.Project, 0, len(projectsData))
	for _, p := range projectsData {
		projects = append(projects, roToRPCMsg(p))
	}

	return connect.NewResponse(&projectsv1.BatchGetResponse{Projects: projects}), nil
}

// Create creates a new project for the authenticated customer.
func (s *server) Create(
	ctx context.Context,
	req *connect.Request[projectsv1.CreateRequest],
) (*connect.Response[projectsv1.CreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.CreateProjectParams{
		ApiKey:      xid.New().String(),
		CustomerID:  project.CustomerID,
		DisplayName: req.Msg.DisplayName,
		ID:          xid.New().String(),
	}
	projectData, err := s.service.CreateProject(ctx, wParams)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slog.Any("error", err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.CreateResponse{Project: wToRPCMsg(projectData)}), nil
}

// Delete removes a project for the authenticated customer.
// todo - this should have a verification step
func (s *server) Delete(
	ctx context.Context,
	req *connect.Request[projectsv1.DeleteRequest],
) (*connect.Response[projectsv1.DeleteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.DeleteProjectParams{
		CustomerID: project.CustomerID,
		ID:         req.Msg.Id,
	}

	if err := s.service.DeleteProject(ctx, wParams); err != nil {
		slog.ErrorContext(ctx, "failed deleting project", slog.Any("error", err), slog.String("customerId", project.CustomerID), slog.String("id", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.DeleteResponse{}), nil
}

// UpdateDisplayName updates the display name of a project for the authenticated customer.
func (s *server) UpdateDisplayName(
	ctx context.Context,
	req *connect.Request[projectsv1.UpdateDisplayNameRequest],
) (*connect.Response[projectsv1.UpdateDisplayNameResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.UpdateProjectDisplayNameParams{CustomerID: project.CustomerID, DisplayName: req.Msg.DisplayName, ID: req.Msg.Id}
	projectData, err := s.service.UpdateProjectDisplayName(ctx, wParams)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slog.Any("error", err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.UpdateDisplayNameResponse{Project: wToRPCMsg(projectData)}), nil
}

// UpdateFCMServiceJSON updates the FCM service JSON for a project.
func (s *server) UpdateFCMServiceJSON(
	ctx context.Context,
	req *connect.Request[projectsv1.UpdateFCMServiceJSONRequest],
) (*connect.Response[projectsv1.UpdateFCMServiceJSONResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.UpdateFCMServiceJSONParams{CustomerID: project.CustomerID, FcmServiceJson: postgres.StringToText(req.Msg.FcmServiceJson), ID: req.Msg.Id}
	if _, err := s.service.UpdateFCMServiceJSON(ctx, wParams); err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slog.Any("err", err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.UpdateFCMServiceJSONResponse{}), nil
}
