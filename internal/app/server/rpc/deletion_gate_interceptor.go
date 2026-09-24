package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/deletion"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
)

var projectWriteProcedures = map[string]struct{}{
	projectsv1connect.ProjectsServiceUpdateMetaProcedure:           {},
	projectsv1connect.ProjectsServiceUpdateFCMServiceJSONProcedure: {},
	projectsv1connect.ProjectsServiceCreateApiKeyProcedure:         {},
	projectsv1connect.ProjectsServiceDeleteApiKeyProcedure:         {},
	dashboardsv1connect.DashboardsServiceCreateProcedure:           {},
	dashboardsv1connect.DashboardsServiceUpdateProcedure:           {},
	dashboardsv1connect.DashboardsServiceDeleteProcedure:           {},
	dashboardsv1connect.DashboardsServiceUpsertProcedure:           {},
	profilesv1connect.ProfilesServiceDeleteProcedure:               {},
	profilesv1connect.ProfilesServiceDeleteDataSubjectProcedure:    {},
	sdkprofilesv1connect.ProfilesSDKServiceIdentifyProcedure:       {},
	eventsv1connect.EventsServiceBatchCreateProcedure:              {},
}

func isProjectWriteProcedure(procedure string) bool {
	_, ok := projectWriteProcedures[procedure]
	return ok
}

// ProjectGateInterceptor keeps project-scoped writes inside the same
// distributed fence that delayed workers use. Reads do not hold a PostgreSQL
// connection, and the deletion entry point acquires the exclusive fence itself.
func ProjectGateInterceptor(gate *deletion.Gate) connect.Interceptor {
	return &projectGateInterceptor{gate: gate}
}

type projectGateInterceptor struct{ gate *deletion.Gate }

func (i *projectGateInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !isProjectWriteProcedure(req.Spec().Procedure) {
			return next(ctx, req)
		}
		principal, err := MustGetPrincipalWithProject(ctx)
		if err != nil {
			return next(ctx, req)
		}
		var result connect.AnyResponse
		err = i.gate.WithActiveProject(ctx, principal.Project.ID, func(ctx context.Context) error {
			var callErr error
			result, callErr = next(ctx, req)
			return callErr
		})
		if errors.Is(err, deletion.ErrProjectInactive) {
			return nil, apperr.NotFound(apperr.ReasonProjectNotFound, "project not found")
		}
		return result, err
	}
}

func (i *projectGateInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *projectGateInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
