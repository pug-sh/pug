package rpc

import (
	"fmt"
	"reflect"

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
// server.go), paired with its generated handler interface. servedProcedures
// derives the served procedures from these interfaces by reflection, so adding,
// renaming, or removing an RPC method is picked up automatically — no hand-kept
// procedure list to drift. This is the authoritative procedure-level source for
// the "no RPC ships without an authz decision" contract, enforced both at startup
// (AssertRegistryMatchesServedProcedures, from server.start) and in CI
// (TestPermissionRegistryCoversAllProcedures).
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

// servedProcedures returns the set of every RPC procedure the server can serve,
// derived by reflection from the generated handler interfaces in servedServices
// (one method per procedure, named "/<service>/<Method>").
func servedProcedures() map[string]bool {
	procs := map[string]bool{}
	for _, svc := range servedServices {
		for i := 0; i < svc.handler.NumMethod(); i++ {
			procs["/"+svc.name+"/"+svc.handler.Method(i).Name] = true
		}
	}
	return procs
}

// AssertRegistryMatchesServedProcedures verifies, at the PROCEDURE level, that the
// permission registry is exactly the set of served procedures: every served
// procedure has an authz decision, and no registry entry is stale. server.start
// calls it so a procedure added to an already-mounted service — which the
// service-level assertServedServicesMatch cannot see — fails startup fast instead
// of relying solely on AuthzInterceptor's runtime fail-closed. It is the startup
// twin of TestPermissionRegistryCoversAllProcedures (both read servedProcedures),
// so CI and boot enforce the same contract.
func AssertRegistryMatchesServedProcedures() error {
	return assertRegistryCoversProcedures(servedProcedures(), permissionRegistry)
}

// assertRegistryCoversProcedures is the pure, injectable core of the check: it
// asserts the two procedure sets are equal in both directions. Split out so the
// rejection paths are unit-tested with synthetic inputs (the exported wrapper binds
// the real served set + registry).
func assertRegistryCoversProcedures(served map[string]bool, registry map[string]authzspec.Spec) error {
	for proc := range served {
		if _, ok := registry[proc]; !ok {
			return fmt.Errorf("server: served RPC %q has no permission-registry entry (no authz decision)", proc)
		}
	}
	for proc := range registry {
		if !served[proc] {
			return fmt.Errorf("server: permission registry has stale entry %q (no such served RPC)", proc)
		}
	}
	return nil
}
