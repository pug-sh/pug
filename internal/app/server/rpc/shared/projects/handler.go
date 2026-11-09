// Package projects serves shared.projects.v1, the projects a credential can read.
// /mcp exposes it as list_projects.
package projects

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	coremcpgrants "github.com/pug-sh/pug/internal/core/mcpgrants"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	sharedprojectsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1/sharedprojectsv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

type Server struct {
	sharedprojectsv1connect.UnimplementedProjectsServiceHandler
	queries *dbread.Queries
	grants  *coremcpgrants.Service
}

// NewServer takes a nil grants service when OAuth is off, which leaves no MCP token
// to list for.
func NewServer(queries *dbread.Queries, grants *coremcpgrants.Service) *Server {
	return &Server{queries: queries, grants: grants}
}

// List answers for the credential: an MCP token gets its grant's projects, a
// dashboard session every project in its orgs, and an API key its own project.
func (s *Server) List(
	ctx context.Context,
	_ *connect.Request[sharedprojectsv1.ListRequest],
) (*connect.Response[sharedprojectsv1.ListResponse], error) {
	var (
		projects []*sharedprojectsv1.Project
		err      error
	)
	if principal, perr := rpc.MustGetPrincipalWithCustomer(ctx); perr == nil {
		if principal.GrantID != "" {
			projects, err = s.grantProjects(ctx, principal.GrantID)
		} else {
			projects, err = s.customerProjects(ctx, principal.Customer.ID)
		}
	} else {
		principal, perr := rpc.MustGetPrincipalWithProject(ctx)
		if perr != nil {
			return nil, perr
		}
		projects, err = s.keyProject(ctx, principal.Project)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&sharedprojectsv1.ListResponse{Projects: projects}), nil
}

func (s *Server) grantProjects(ctx context.Context, grantID string) ([]*sharedprojectsv1.Project, error) {
	projects, err := s.grants.ListProjects(ctx, grantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	out := make([]*sharedprojectsv1.Project, len(projects))
	for i, p := range projects {
		out[i] = projectToRPC(p.ID, p.DisplayName, p.OrgID, p.OrgDisplayName)
	}
	return out, nil
}

func (s *Server) customerProjects(ctx context.Context, customerID string) ([]*sharedprojectsv1.Project, error) {
	rows, err := s.queries.GetProjectsWithOrgByCustomerID(ctx, customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the customer's projects", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	out := make([]*sharedprojectsv1.Project, len(rows))
	for i, r := range rows {
		out[i] = projectToRPC(r.ID, r.DisplayName, r.OrgID, r.OrgDisplayName)
	}
	return out, nil
}

func (s *Server) keyProject(ctx context.Context, project *dbread.Project) ([]*sharedprojectsv1.Project, error) {
	org, err := s.queries.GetOrgByID(ctx, project.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the key's org", slogx.Error(err), slog.String("project_id", project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return []*sharedprojectsv1.Project{projectToRPC(project.ID, project.DisplayName, org.ID, org.DisplayName)}, nil
}

func projectToRPC(id, displayName, orgID, orgDisplayName string) *sharedprojectsv1.Project {
	return &sharedprojectsv1.Project{
		Id:             proto.String(id),
		DisplayName:    proto.String(displayName),
		OrgId:          proto.String(orgID),
		OrgDisplayName: proto.String(orgDisplayName),
	}
}
