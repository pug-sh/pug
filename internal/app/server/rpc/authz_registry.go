package rpc

import (
	"sort"
	"strings"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
	"github.com/pug-sh/pug/internal/core/authz"
)

// permissionRegistry maps every served RPC procedure to its authz decision
// (authzspec.Spec). Each Spec is built by a constructor — Public/Self/Project/
// SDKKey for the non-gated kinds, and OrgGated/ProjGated for the role-gated ones
// (which the interceptor enforces). Because authzspec.Spec's fields are
// unexported, a role-gated entry cannot be written as a bare literal that forgets
// orgSource, and a non-gated literal cannot carry a stray (resource, action) — so
// "role-gated ⟺ resource+action+orgSource" holds by construction.
//
// OrgGated resolves the caller's org from the request message's GetOrgId (org
// control plane); ProjGated resolves it from the x-project-id project
// (principal.Project.OrgID — project data plane + project-lifecycle writes).
//
// TestPermissionRegistryCoversAllProcedures asserts this map is exactly the set
// of served procedures (no missing entry — "no RPC ships without an authz
// decision" — and no stale entry), deriving the truth from the generated handler
// interfaces via reflection.
var permissionRegistry = map[string]authzspec.Spec{
	// --- public.auth.v1.AuthService — no auth ---
	"/public.auth.v1.AuthService/SignInWithEmail":     authzspec.Public(),
	"/public.auth.v1.AuthService/RequestMagicLink":    authzspec.Public(),
	"/public.auth.v1.AuthService/CompleteMagicLink":   authzspec.Public("invite acceptance is authorized by invite-token possession, not an org role"),
	"/public.auth.v1.AuthService/CompleteOAuthSignIn": authzspec.Public(),
	"/public.auth.v1.AuthService/RefreshSession":      authzspec.Public("runs after access-token expiry; authorized by refresh-token possession"),
	"/public.auth.v1.AuthService/SignOut":             authzspec.Public(),

	// --- public.dashboards.v1.SharedDashboardsService — no auth (share token) ---
	"/public.dashboards.v1.SharedDashboardsService/Query": authzspec.Public("authorized by share_id"),

	// --- dashboard.orgs.v1.OrgsService ---
	"/dashboard.orgs.v1.OrgsService/List":              authzspec.Self("returns only the caller's orgs"),
	"/dashboard.orgs.v1.OrgsService/Create":            authzspec.Self("any authenticated customer may create an org"),
	"/dashboard.orgs.v1.OrgsService/Leave":             authzspec.Self("self-service; last-admin/last-member guards live in the service"),
	"/dashboard.orgs.v1.OrgsService/Get":               authzspec.OrgGated(authz.ResourceOrg, authz.ActionRead, "non-members are denied identically whether or not the org exists, so existence stays hidden"),
	"/dashboard.orgs.v1.OrgsService/ListMembers":       authzspec.OrgGated(authz.ResourceMember, authz.ActionRead),
	"/dashboard.orgs.v1.OrgsService/UpdateDisplayName": authzspec.OrgGated(authz.ResourceOrg, authz.ActionUpdate),
	"/dashboard.orgs.v1.OrgsService/InviteMember":      authzspec.OrgGated(authz.ResourceInvitation, authz.ActionCreate),
	"/dashboard.orgs.v1.OrgsService/ResendInvite":      authzspec.OrgGated(authz.ResourceInvitation, authz.ActionUpdate),
	"/dashboard.orgs.v1.OrgsService/ListInvitations":   authzspec.OrgGated(authz.ResourceInvitation, authz.ActionRead),
	"/dashboard.orgs.v1.OrgsService/RemoveMember":      authzspec.OrgGated(authz.ResourceMember, authz.ActionDelete),
	"/dashboard.orgs.v1.OrgsService/UpdateMemberRole":  authzspec.OrgGated(authz.ResourceMember, authz.ActionUpdate),

	// --- dashboard.projects.v1.ProjectsService ---
	"/dashboard.projects.v1.ProjectsService/BatchGet":             authzspec.OrgGated(authz.ResourceProject, authz.ActionRead),
	"/dashboard.projects.v1.ProjectsService/Create":               authzspec.OrgGated(authz.ResourceProject, authz.ActionCreate, "interceptor is the coarse gate; the authoritative admin check is race-safe in the CreateProjectAsAdmin CTE"),
	"/dashboard.projects.v1.ProjectsService/Get":                  authzspec.Project(),
	"/dashboard.projects.v1.ProjectsService/Delete":               authzspec.ProjGated(authz.ResourceProject, authz.ActionDelete, "admin-only; org resolved from the x-project-id project"),
	"/dashboard.projects.v1.ProjectsService/UpdateMeta":           authzspec.ProjGated(authz.ResourceProject, authz.ActionUpdate, "admin-only; org resolved from the x-project-id project"),
	"/dashboard.projects.v1.ProjectsService/UpdateFCMServiceJSON": authzspec.ProjGated(authz.ResourceProject, authz.ActionUpdate, "admin-only; org resolved from the x-project-id project"),

	// --- dashboard.dashboards.v1.DashboardsService — project-data plane (JWT + x-project-id) ---
	"/dashboard.dashboards.v1.DashboardsService/Get":            authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/List":           authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/QueryDashboard": authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/Create":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionCreate),
	"/dashboard.dashboards.v1.DashboardsService/Update":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionUpdate),
	"/dashboard.dashboards.v1.DashboardsService/Delete":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionDelete),
	"/dashboard.dashboards.v1.DashboardsService/Upsert":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionUpdate, "reconciles tiles of an existing dashboard"),

	// --- dashboard.orgemailproviders.v1.OrgEmailProvidersService — admin-gated (email_provider is admin-only in the policy) ---
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Get":      authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionRead),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Set":      authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionUpdate),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Remove":   authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionDelete),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/SendTest": authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionUpdate),

	// --- dashboard.customers.v1.CustomersService — self-service ---
	"/dashboard.customers.v1.CustomersService/GetMe":       authzspec.Self(),
	"/dashboard.customers.v1.CustomersService/SetPassword": authzspec.Self(),

	// --- shared.insights.v1.InsightsService — project-data plane (JWT or private key) ---
	"/shared.insights.v1.InsightsService/Query":             authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/SegmentUsers":      authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/GetFilterSchema":   authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/GetPropertyValues": authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),

	// --- shared.activity.v1.ActivityService — project-data plane ---
	"/shared.activity.v1.ActivityService/GetActivityFeed":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetEventExplorer":   authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetFilterSchema":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetPropertyValues":  authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetActivityHeatmap": authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetProfileStats":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),

	// --- shared.profiles.v1.ProfilesService — project-data plane ---
	"/shared.profiles.v1.ProfilesService/Get":                authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/GetByExternalId":    authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/List":               authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/GetDeletionRequest": authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/Delete":             authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete),
	"/shared.profiles.v1.ProfilesService/DeleteDataSubject":  authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete, "GDPR/DPDP erasure; member+ on the JWT path, coarse on private key"),

	// --- sdk.profiles.v1.ProfilesSDKService — API key ---
	"/sdk.profiles.v1.ProfilesSDKService/Identify": authzspec.SDKKey(),

	// --- sdk.events.v1.EventsService — API key ---
	"/sdk.events.v1.EventsService/BatchCreate": authzspec.SDKKey(),
}

// ServedServiceNames returns the distinct RPC service names that appear in the
// permission registry (e.g. "dashboard.orgs.v1.OrgsService"), sorted. Because
// TestPermissionRegistryCoversAllProcedures pins the registry to exactly the set
// of served procedures, this is the authoritative "what is served" list:
// server.start uses it both to advertise gRPC reflection and to assert (via
// assertServedServicesMatch) that every mounted RPC service has an authz decision.
func ServedServiceNames() []string {
	seen := map[string]struct{}{}
	var names []string
	for proc := range permissionRegistry {
		// proc is "/<service>/<method>"; take the <service> segment.
		trimmed := strings.TrimPrefix(proc, "/")
		slash := strings.LastIndexByte(trimmed, '/')
		if slash <= 0 {
			continue
		}
		svc := trimmed[:slash]
		if _, ok := seen[svc]; !ok {
			seen[svc] = struct{}{}
			names = append(names, svc)
		}
	}
	sort.Strings(names)
	return names
}
