package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/tzx"
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
		return nil, err
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
		return nil, err
	}

	orgID := req.Msg.GetOrgId()
	isMember, err := s.orgsService.IsOrgMember(ctx, orgID, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err), slog.String("org_id", orgID), slog.String("customer_id", principal.Customer.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, apperr.PermissionDenied(apperr.ReasonOrgNotAMember, "not a member of this org")
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
		return nil, err
	}

	projectData, err := s.service.CreateProjectAsAdmin(ctx, req.Msg.GetOrgId(), principal.Customer.ID, req.Msg.GetDisplayName(), req.Msg.GetReportingTimezone())
	if err != nil {
		if errors.Is(err, projects.ErrAdminRequired) {
			return nil, apperr.PermissionDenied(apperr.ReasonOrgAdminRequired, "admin role required")
		}
		if errors.Is(err, projects.ErrProjectNameTaken) {
			return nil, apperr.AlreadyExists(apperr.ReasonProjectNameTaken, "a project with this name already exists")
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
		return nil, err
	}

	wParams := dbwrite.DeleteProjectParams{
		OrgID: principal.Project.OrgID,
		ID:    principal.Project.ID,
	}

	if err := s.service.DeleteProject(ctx, wParams); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProjectNotFound, "project not found", apperr.Resource("project", principal.Project.ID))
		}
		slog.ErrorContext(ctx, "failed deleting project", slogx.Error(err), slog.String("org_id", principal.Project.OrgID), slog.String("id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.DeleteResponse{}), nil
}

// UpdateMeta partially updates the editable metadata (display name + reporting
// timezone) of the project specified by the x-project-id header. An omitted field
// is left unchanged; a present reporting_timezone of "" resets it to UTC.
func (s *server) UpdateMeta(
	ctx context.Context,
	req *connect.Request[projectsv1.UpdateMetaRequest],
) (*connect.Response[projectsv1.UpdateMetaResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	// Partial update: each field is presence-tracked (edition 2023). A nil pointer
	// means "leave unchanged" — NewNullableText emits SQL NULL and the query's
	// coalesce(...) preserves the stored value. reporting_timezone is validated and
	// normalized only when the client actually sent it; a present "" resets to UTC.
	var tz *string
	if req.Msg.ReportingTimezone != nil {
		if err := tzx.Validate(*req.Msg.ReportingTimezone); err != nil {
			return nil, apperr.Invalid(apperr.ReasonInvalidTimezone,
				fmt.Sprintf("invalid timezone %q", *req.Msg.ReportingTimezone))
		}
		n := tzx.Normalize(*req.Msg.ReportingTimezone)
		tz = &n
	}

	wParams := dbwrite.UpdateProjectMetaParams{
		OrgID:             principal.Project.OrgID,
		ID:                principal.Project.ID,
		DisplayName:       postgres.NewNullableText(req.Msg.DisplayName),
		ReportingTimezone: postgres.NewNullableText(tz),
	}
	projectData, err := s.service.UpdateProjectMeta(ctx, wParams)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProjectNotFound, "project not found", apperr.Resource("project", wParams.ID))
		}
		slog.ErrorContext(ctx, "failed to update project meta", slogx.Error(err), slog.String("project_id", wParams.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.UpdateMetaResponse{Project: wToRPCMsg(projectData)}), nil
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
		return nil, err
	}

	wParams := dbwrite.UpdateFCMServiceJSONParams{
		OrgID:          principal.Project.OrgID,
		FcmServiceJson: postgres.NewText(req.Msg.GetFcmServiceJson()),
		ID:             principal.Project.ID,
	}
	if _, err := s.service.UpdateFCMServiceJSON(ctx, wParams); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProjectNotFound, "project not found", apperr.Resource("project", wParams.ID))
		}
		slog.ErrorContext(ctx, "failed to update project FCM service JSON", slogx.Error(err), slog.String("project_id", wParams.ID), slog.String("org_id", wParams.OrgID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.UpdateFCMServiceJSONResponse{}), nil
}
