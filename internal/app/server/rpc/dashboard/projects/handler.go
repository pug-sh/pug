package projects

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	orgID := req.Msg.GetOrgId()
	isMember, err := s.orgsService.IsOrgMember(ctx, orgID, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err), slog.String("org_id", orgID), slog.String("customer_id", principal.Customer.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	projectsData, err := s.service.GetProjectsByOrgID(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed reading from db", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectData, err := s.service.CreateProjectAsAdmin(ctx, req.Msg.GetOrgId(), principal.Customer.ID, req.Msg.GetDisplayName())
	if err != nil {
		if errors.Is(err, projects.ErrAdminRequired) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
		}
		if errors.Is(err, projects.ErrProjectNameTaken) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("a project with this name already exists"))
		}
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	wParams := dbwrite.DeleteProjectParams{
		OrgID: principal.Project.OrgID,
		ID:    principal.Project.ID,
	}

	if err := s.service.DeleteProject(ctx, wParams); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
		}
		slog.ErrorContext(ctx, "failed deleting project", slogx.Error(err), slog.String("org_id", principal.Project.OrgID), slog.String("id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	wParams := dbwrite.UpdateProjectDisplayNameParams{OrgID: principal.Project.OrgID, DisplayName: req.Msg.GetDisplayName(), ID: principal.Project.ID}
	projectData, err := s.service.UpdateProjectDisplayName(ctx, wParams)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
		}
		slog.ErrorContext(ctx, "failed to update project display name", slogx.Error(err), slog.String("project_id", wParams.ID))
		telemetry.RecordError(ctx, err)
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	wParams := dbwrite.UpdateFCMServiceJSONParams{
		OrgID:          principal.Project.OrgID,
		FcmServiceJson: postgres.NewText(req.Msg.GetFcmServiceJson()),
		ID:             principal.Project.ID,
	}
	if _, err := s.service.UpdateFCMServiceJSON(ctx, wParams); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
		}
		slog.ErrorContext(ctx, "failed to update project FCM service JSON", slogx.Error(err), slog.String("project_id", wParams.ID), slog.String("org_id", wParams.OrgID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.UpdateFCMServiceJSONResponse{}), nil
}
