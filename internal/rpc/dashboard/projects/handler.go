package projects

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/projects"
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/rpc"
	"github.com/fivebitsio/cotton/internal/deps/logger/slogx"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

type server struct {
	service *projects.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *server {
	service := projects.NewService(pgRO, pgW)

	return &server{
		service: service,
	}
}

// Get returns the project specified by x-project-id header.
func (s *server) Get(
	ctx context.Context,
	_ *connect.Request[projectsv1.GetRequest],
) (*connect.Response[projectsv1.GetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if principal.Project == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}

	return connect.NewResponse(&projectsv1.GetResponse{Project: roToRPCMsg(*principal.Project)}), nil
}

// BatchGet returns all projects for the authenticated customer.
func (s *server) BatchGet(
	ctx context.Context,
	_ *connect.Request[projectsv1.BatchGetRequest],
) (*connect.Response[projectsv1.BatchGetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	projectsData, err := s.service.GetProjectsByCustomerID(ctx, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed reading from db", slogx.Error(err), slog.String("customerId", principal.Customer.ID))
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

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.CreateProjectParams{
		ApiKey:      xid.New().String(),
		CustomerID:  principal.Customer.ID,
		DisplayName: req.Msg.DisplayName,
		ID:          xid.New().String(),
	}

	projectData, err := s.service.CreateProject(ctx, wParams)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.CreateResponse{Project: wToRPCMsg(projectData)}), nil
}

// Delete removes the project specified by x-project-id header.
func (s *server) Delete(
	ctx context.Context,
	_ *connect.Request[projectsv1.DeleteRequest],
) (*connect.Response[projectsv1.DeleteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if principal.Project == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}

	wParams := dbwrite.DeleteProjectParams{
		CustomerID: principal.Customer.ID,
		ID:         principal.Project.ID,
	}

	if err := s.service.DeleteProject(ctx, wParams); err != nil {
		slog.ErrorContext(ctx, "failed deleting project", slogx.Error(err), slog.String("customerId", principal.Customer.ID), slog.String("id", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.DeleteResponse{}), nil
}

// UpdateDisplayName updates the display name of the project specified by x-project-id header.
func (s *server) UpdateDisplayName(
	ctx context.Context,
	req *connect.Request[projectsv1.UpdateDisplayNameRequest],
) (*connect.Response[projectsv1.UpdateDisplayNameResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if principal.Project == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}

	wParams := dbwrite.UpdateProjectDisplayNameParams{CustomerID: principal.Customer.ID, DisplayName: req.Msg.DisplayName, ID: principal.Project.ID}
	projectData, err := s.service.UpdateProjectDisplayName(ctx, wParams)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.UpdateDisplayNameResponse{Project: wToRPCMsg(projectData)}), nil
}

// UpdateFCMServiceJSON updates the FCM service JSON for the project specified by x-project-id header.
func (s *server) UpdateFCMServiceJSON(
	ctx context.Context,
	req *connect.Request[projectsv1.UpdateFCMServiceJSONRequest],
) (*connect.Response[projectsv1.UpdateFCMServiceJSONResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if principal.Project == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}

	wParams := dbwrite.UpdateFCMServiceJSONParams{CustomerID: principal.Customer.ID, FcmServiceJson: postgres.StringToText(req.Msg.FcmServiceJson), ID: principal.Project.ID}
	if _, err := s.service.UpdateFCMServiceJSON(ctx, wParams); err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectsv1.UpdateFCMServiceJSONResponse{}), nil
}
