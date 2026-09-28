package rpc

import (
	"testing"

	"github.com/pug-sh/pug/internal/gen/proto/dashboard/instance/v1/instancev1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
)

func TestDeletionGateProcedureScopes(t *testing.T) {
	tests := []struct {
		name      string
		procedure string
		project   bool
		org       bool
	}{
		{"SDK event write", eventsv1connect.EventsServiceBatchCreateProcedure, true, false},
		{"project metadata write", projectsv1connect.ProjectsServiceUpdateMetaProcedure, true, false},
		{"project read", insightsv1connect.InsightsServiceQueryProcedure, false, false},
		{"project deletion owns exclusive gate", projectsv1connect.ProjectsServiceDeleteProcedure, false, false},
		{"organization member write", orgsv1connect.OrgsServiceRemoveMemberProcedure, false, true},
		{"instance organization write", instancev1connect.InstanceAdminServiceRenameOrganizationProcedure, false, true},
		{"organization read", orgsv1connect.OrgsServiceGetProcedure, false, false},
		{"deletion history read", projectsv1connect.ProjectsServiceListDeletionsProcedure, false, false},
		{"deletion retry", projectsv1connect.ProjectsServiceRetryDeletionProcedure, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProjectWriteProcedure(tc.procedure); got != tc.project {
				t.Errorf("project gate = %v, want %v", got, tc.project)
			}
			if got := isOrganizationWriteProcedure(tc.procedure); got != tc.org {
				t.Errorf("organization gate = %v, want %v", got, tc.org)
			}
		})
	}
}
