package projects

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/core/orgs"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
)

type server struct {
	service     *projects.Service
	orgsService *orgs.Service
}

func NewServer(service *projects.Service, orgsService *orgs.Service) *server {
	return &server{service: service, orgsService: orgsService}
}

// Get returns the project specified by x-project-id header.
func (s *server) Get(
	ctx context.Context,
	_ *connect.Request[projectsv1.GetRequest],
) (*connect.Response[projectsv1.GetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	return connect.NewResponse(&projectsv1.GetResponse{Project: roToRPCMsg(*principal.Project)}), nil
}

// BatchGet returns all projects for the given org.
func (s *server) BatchGet(
	ctx context.Context,
	req *connect.Request[projectsv1.BatchGetRequest],
) (*connect.Response[projectsv1.BatchGetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isMember, err := s.orgsService.IsOrgMember(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	projectsData, err := s.service.GetProjectsByOrgID(ctx, req.Msg.OrgId)
	if err != nil {
		slog.ErrorContext(ctx, "failed reading from db", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*projectsv1.Project, 0, len(projectsData))
	for _, p := range projectsData {
		result = append(result, roToRPCMsg(p))
	}

	return connect.NewResponse(&projectsv1.BatchGetResponse{Projects: result}), nil
}

// Create creates a new project in the given org.
func (s *server) Create(
	ctx context.Context,
	req *connect.Request[projectsv1.CreateRequest],
) (*connect.Response[projectsv1.CreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isMember, err := s.orgsService.IsOrgMember(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	projectData, err := s.service.CreateProject(ctx, req.Msg.OrgId, req.Msg.DisplayName)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.CreateResponse{Project: wToRPCMsgWithPrivateKey(projectData)}), nil
}

// Delete removes the project specified by x-project-id header.
func (s *server) Delete(
	ctx context.Context,
	_ *connect.Request[projectsv1.DeleteRequest],
) (*connect.Response[projectsv1.DeleteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.DeleteProjectParams{
		OrgID: principal.Project.OrgID,
		ID:    principal.Project.ID,
	}

	if err := s.service.DeleteProject(ctx, wParams); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
		}
		slog.ErrorContext(ctx, "failed deleting project", slogx.Error(err), slog.String("orgId", principal.Project.OrgID), slog.String("id", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
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

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.UpdateProjectDisplayNameParams{OrgID: principal.Project.OrgID, DisplayName: req.Msg.DisplayName, ID: principal.Project.ID}
	projectData, err := s.service.UpdateProjectDisplayName(ctx, wParams)
	if err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err), slog.Any("params", wParams))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
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

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	wParams := dbwrite.UpdateFCMServiceJSONParams{
		OrgID:          principal.Project.OrgID,
		FcmServiceJson: postgres.NewText(req.Msg.FcmServiceJson),
		ID:             principal.Project.ID,
	}
	if _, err := s.service.UpdateFCMServiceJSON(ctx, wParams); err != nil {
		slog.ErrorContext(ctx, "failed writing to db", slogx.Error(err), slog.String("projectID", wParams.ID), slog.String("orgID", wParams.OrgID), slogx.Redacted("fcm_service_json", wParams.FcmServiceJson.String))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.UpdateFCMServiceJSONResponse{}), nil
}
