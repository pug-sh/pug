package rpc

import (
	"sort"
	"strings"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
	"github.com/pug-sh/pug/internal/core/authz"
)

// permissionRegistry maps every served RPC procedure to its authz decision. The
// authzspec constructors are what make "role-gated ⟺ resource+action+orgSource"
// hold by construction (see that package's doc); the optional trailing string is
// a note for the reader.
//
// OrgGated resolves the caller's org from the request message's GetOrgId (org
// control plane); ProjGated resolves it from the x-project-id project
// (principal.Project.OrgID — project data plane + project-lifecycle writes).
//
// TestPermissionRegistryCoversAllProcedures asserts this map is exactly the set
// of served procedures, derived from the generated handler interfaces by
// reflection: no RPC ships without a decision, and no entry outlives its RPC.
var permissionRegistry = map[string]authzspec.Spec{
	// --- public.auth.v1.AuthService ---
	"/public.auth.v1.AuthService/SignInWithEmail":    authzspec.Public(),
	"/public.auth.v1.AuthService/RequestMagicLink":   authzspec.Public(),
	"/public.auth.v1.AuthService/CompleteMagicLink":  authzspec.Public("invite acceptance is authorized by invite-token possession, not an org role"),
	"/public.auth.v1.AuthService/CompleteOIDCSignIn": authzspec.Public(),
	"/public.auth.v1.AuthService/GetAuthConfig":      authzspec.Public(),
	"/public.auth.v1.AuthService/RefreshSession":     authzspec.Public("runs after access-token expiry; authorized by refresh-token possession"),
	"/public.auth.v1.AuthService/SignOut":            authzspec.Public(),
	"/public.auth.v1.AuthService/DemoSignIn":         authzspec.Public("credential-less demo viewer login; gated by PUG_DEMO_ENABLED, and the minted principal is a read-only org viewer"),

	// --- public.dashboards.v1.SharedDashboardsService ---
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
	"/dashboard.orgs.v1.OrgsService/RevokeInvite":      authzspec.OrgGated(authz.ResourceInvitation, authz.ActionDelete),
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
	"/dashboard.projects.v1.ProjectsService/ListApiKeys":          authzspec.ProjGated(authz.ResourceAPIKey, authz.ActionRead, "every role; a private key is only ever returned masked"),
	"/dashboard.projects.v1.ProjectsService/CreateApiKey":         authzspec.ProjGated(authz.ResourceAPIKey, authz.ActionCreate, "admin-only; org resolved from the x-project-id project"),
	"/dashboard.projects.v1.ProjectsService/DeleteApiKey":         authzspec.ProjGated(authz.ResourceAPIKey, authz.ActionDelete, "admin-only; org resolved from the x-project-id project"),

	// --- dashboard.dashboards.v1.DashboardsService ---
	"/dashboard.dashboards.v1.DashboardsService/Get":            authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/List":           authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/QueryDashboard": authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
	"/dashboard.dashboards.v1.DashboardsService/Create":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionCreate),
	"/dashboard.dashboards.v1.DashboardsService/Update":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionUpdate),
	"/dashboard.dashboards.v1.DashboardsService/Delete":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionDelete),
	"/dashboard.dashboards.v1.DashboardsService/Upsert":         authzspec.ProjGated(authz.ResourceDashboard, authz.ActionUpdate, "reconciles tiles of an existing dashboard"),

	// --- dashboard.orgemailproviders.v1.OrgEmailProvidersService — admin-only in the policy, reads included ---
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Get":      authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionRead),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Set":      authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionUpdate),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Remove":   authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionDelete),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/SendTest": authzspec.OrgGated(authz.ResourceEmailProvider, authz.ActionUpdate),

	// --- dashboard.usage.v1.UsageService ---
	"/dashboard.usage.v1.UsageService/GetUsage": authzspec.OrgGated(authz.ResourceUsage, authz.ActionRead),

	// --- dashboard.billing.v1.BillingService ---
	"/dashboard.billing.v1.BillingService/GetBillingStatus":      authzspec.OrgGated(authz.ResourceBilling, authz.ActionRead),
	"/dashboard.billing.v1.BillingService/ListPlans":             authzspec.OrgGated(authz.ResourceBilling, authz.ActionRead, "on the viewer floor: whoever reads the quota banner is who wants to know what the next tier costs"),
	"/dashboard.billing.v1.BillingService/CreateCheckoutSession": authzspec.OrgGated(authz.ResourceBilling, authz.ActionCreate, "admin-only; starting a checkout spends money"),
	"/dashboard.billing.v1.BillingService/CreatePortalSession":   authzspec.OrgGated(authz.ResourceBilling, authz.ActionCreate, "admin-only; the portal reaches invoices"),
	"/dashboard.billing.v1.BillingService/ConfirmCheckout":       authzspec.OrgGated(authz.ResourceBilling, authz.ActionCreate, "admin-only; the other half of starting, and it writes the subscription row"),

	// --- dashboard.customers.v1.CustomersService ---
	"/dashboard.customers.v1.CustomersService/GetMe":       authzspec.Self(),
	"/dashboard.customers.v1.CustomersService/SetPassword": authzspec.Self(),

	// --- shared.insights.v1.InsightsService ---
	"/shared.insights.v1.InsightsService/Query":             authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/SegmentUsers":      authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/GetFilterSchema":   authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
	"/shared.insights.v1.InsightsService/GetPropertyValues": authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),

	// --- shared.activity.v1.ActivityService ---
	"/shared.activity.v1.ActivityService/GetActivityFeed":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetEventExplorer":   authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetFilterSchema":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetPropertyValues":  authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetActivityHeatmap": authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetProfileSessions": authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),
	"/shared.activity.v1.ActivityService/GetProfileStats":    authzspec.ProjGated(authz.ResourceActivity, authz.ActionRead),

	// --- shared.profiles.v1.ProfilesService ---
	"/shared.profiles.v1.ProfilesService/Get":                authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/GetByExternalId":    authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/List":               authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/GetDeletionRequest": authzspec.ProjGated(authz.ResourceProfile, authz.ActionRead),
	"/shared.profiles.v1.ProfilesService/Delete":             authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete),
	"/shared.profiles.v1.ProfilesService/DeleteDataSubject":  authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete, "GDPR/DPDP erasure; member+ on the JWT path, coarse on private key"),

	// --- sdk.profiles.v1.ProfilesSDKService ---
	"/sdk.profiles.v1.ProfilesSDKService/Identify": authzspec.SDKKey(),

	// --- sdk.events.v1.EventsService ---
	"/sdk.events.v1.EventsService/BatchCreate": authzspec.SDKKey(),
}

// ServedServiceNames returns the distinct service names in the registry, sorted.
// Because TestPermissionRegistryCoversAllProcedures pins the registry to exactly
// the served procedures, this is the authoritative "what is served" list:
// server.start uses it both to advertise gRPC reflection and to assert (via
// assertServedServicesMatch) that every mounted service has an authz decision.
func ServedServiceNames() []string {
	seen := map[string]struct{}{}
	var names []string
	for proc := range permissionRegistry {
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
