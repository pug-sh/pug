package rpc

import (
	"reflect"
	"testing"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1/customersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1/orgemailprovidersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/public/auth/v1/authv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1/publicdashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
)

// servedServices lists every RPC service mounted by the server (see start() in
// server.go), paired with its generated handler interface. The contract test
// derives the served procedures from these interfaces by reflection, so adding,
// renaming, or removing an RPC method is caught automatically — no hand-kept
// procedure list to drift.
var servedServices = []struct {
	name    string
	handler reflect.Type
}{
	{authv1connect.AuthServiceName, reflect.TypeFor[authv1connect.AuthServiceHandler]()},
	{publicdashboardsv1connect.SharedDashboardsServiceName, reflect.TypeFor[publicdashboardsv1connect.SharedDashboardsServiceHandler]()},
	{orgsv1connect.OrgsServiceName, reflect.TypeFor[orgsv1connect.OrgsServiceHandler]()},
	{projectsv1connect.ProjectsServiceName, reflect.TypeFor[projectsv1connect.ProjectsServiceHandler]()},
	{dashboardsv1connect.DashboardsServiceName, reflect.TypeFor[dashboardsv1connect.DashboardsServiceHandler]()},
	{orgemailprovidersv1connect.OrgEmailProvidersServiceName, reflect.TypeFor[orgemailprovidersv1connect.OrgEmailProvidersServiceHandler]()},
	{customersv1connect.CustomersServiceName, reflect.TypeFor[customersv1connect.CustomersServiceHandler]()},
	{insightsv1connect.InsightsServiceName, reflect.TypeFor[insightsv1connect.InsightsServiceHandler]()},
	{activityv1connect.ActivityServiceName, reflect.TypeFor[activityv1connect.ActivityServiceHandler]()},
	{profilesv1connect.ProfilesServiceName, reflect.TypeFor[profilesv1connect.ProfilesServiceHandler]()},
	{sdkprofilesv1connect.ProfilesSDKServiceName, reflect.TypeFor[sdkprofilesv1connect.ProfilesSDKServiceHandler]()},
	{eventsv1connect.EventsServiceName, reflect.TypeFor[eventsv1connect.EventsServiceHandler]()},
}

func servedProcedures() map[string]bool {
	procs := map[string]bool{}
	for _, svc := range servedServices {
		for i := 0; i < svc.handler.NumMethod(); i++ {
			procs["/"+svc.name+"/"+svc.handler.Method(i).Name] = true
		}
	}
	return procs
}

// TestPermissionRegistryCoversAllProcedures is the "no RPC ships without an
// authz decision" contract: every served procedure must have a registry entry,
// and every registry entry must correspond to a served procedure.
func TestPermissionRegistryCoversAllProcedures(t *testing.T) {
	served := servedProcedures()

	for proc := range served {
		if _, ok := permissionRegistry[proc]; !ok {
			t.Errorf("served RPC %q has no permissionRegistry entry — add an explicit authz decision", proc)
		}
	}
	for proc := range permissionRegistry {
		if !served[proc] {
			t.Errorf("permissionRegistry has stale entry %q (no such served RPC)", proc)
		}
	}
}

// TestPermissionRegistryRoleEntriesAreComplete asserts every entry is built by an
// authzspec constructor (not a bare Spec{}) and that every role-gated entry
// carries the full triple the interceptor enforces (resource + action +
// orgSource). The reverse — a non-role-gated entry carrying a stray triple — is
// now impossible to express (authzspec.Spec's fields are unexported and only the
// constructors set them), so it needs no check.
func TestPermissionRegistryRoleEntriesAreComplete(t *testing.T) {
	for proc, spec := range permissionRegistry {
		if !spec.Defined() {
			t.Errorf("registry entry %q is an undefined (zero) Spec — build it with an authzspec constructor", proc)
		}
		if !spec.IsRoleGated() {
			continue
		}
		if spec.Resource() == "" || spec.Action() == "" {
			t.Errorf("role-gated entry %q must set resource and action", proc)
		}
		if spec.OrgSource() != authzspec.OrgFromMessage && spec.OrgSource() != authzspec.OrgFromProject {
			t.Errorf("role-gated entry %q must set a valid orgSource", proc)
		}
	}
}
