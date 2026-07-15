package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/tzx"
)

// Authorization (project-role gating) is enforced centrally by
// rpc.AuthzInterceptor from the permission registry, before any handler runs —
// project lifecycle writes are admin-only, BatchGet is member+, all resolved
// from the registry. Create additionally has a race-safe admin check in its CTE.
type server struct {
	service *projects.Service
}

func NewServer(service *projects.Service) *server {
	return &server{service: service}
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

	orgID := req.Msg.GetOrgId()
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

	return connect.NewResponse(&projectsv1.CreateResponse{Project: wToRPCMsg(projectData)}), nil
}

// ListApiKeys returns the API keys of the project specified by the x-project-id
// header. Public keys are returned in full; a private key is only ever a mask.
func (s *server) ListApiKeys(
	ctx context.Context,
	_ *connect.Request[projectsv1.ListApiKeysRequest],
) (*connect.Response[projectsv1.ListApiKeysResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	keys, err := s.service.ListApiKeys(ctx, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed listing api keys", slogx.Error(err), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*projectsv1.ApiKey, 0, len(keys))
	for _, k := range keys {
		result = append(result, apiKeyToRPCMsg(k))
	}

	return connect.NewResponse(&projectsv1.ListApiKeysResponse{ApiKeys: result}), nil
}

// CreateApiKey mints a key for the project specified by the x-project-id header.
// The response carries the key in full — for a private key that is the only time
// it exists outside the caller, since only its digest is stored.
func (s *server) CreateApiKey(
	ctx context.Context,
	req *connect.Request[projectsv1.CreateApiKeyRequest],
) (*connect.Response[projectsv1.CreateApiKeyResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	kind, ok := kindFromRPCEnum(req.Msg.GetKind())
	if !ok {
		// Unreachable: the request is protovalidated defined_only + not_in [0].
		// Reaching this means a new enum value shipped without a mapping.
		err := fmt.Errorf("unmapped api key kind %v", req.Msg.GetKind())
		slog.ErrorContext(ctx, "failed to map api key kind", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	created, err := s.service.CreateApiKey(ctx, principal.Project.ID, kind, req.Msg.GetDisplayName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.CreateApiKeyResponse{
		ApiKey: createdApiKeyToRPCMsg(created.Key),
		Key:    proto.String(created.RawKey),
	}), nil
}

// DeleteApiKey revokes a key of the project specified by the x-project-id header.
// It takes effect immediately; the project keeps working through its other keys.
func (s *server) DeleteApiKey(
	ctx context.Context,
	req *connect.Request[projectsv1.DeleteApiKeyRequest],
) (*connect.Response[projectsv1.DeleteApiKeyResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.DeleteApiKey(ctx, principal.Project.ID, req.Msg.GetId()); err != nil {
		if errors.Is(err, projects.ErrApiKeyNotFound) {
			return nil, apperr.NotFound(apperr.ReasonApiKeyNotFound, "API key not found", apperr.Resource("api_key", req.Msg.GetId()))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&projectsv1.DeleteApiKeyResponse{}), nil
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
