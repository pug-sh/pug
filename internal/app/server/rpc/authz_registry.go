package rpc

import (
	"sort"
	"strings"

	"github.com/pug-sh/pug/internal/core/authz"
)

// authzDomain classifies how an RPC is authorized — i.e. where its authorization
// boundary lives. It is documentation + a coverage contract, not a runtime
// dispatcher: enforcement stays explicit in each handler (a generic interceptor
// is not viable because the org_id provenance is split between the request
// message and the x-project-id header, and several handlers deliberately return
// NotFound rather than PermissionDenied to avoid leaking existence).
type authzDomain int

const (
	// domainPublic: no authentication (the auth service; public share links).
	domainPublic authzDomain = iota
	// domainSelf: authenticated customer operating on their OWN data / orgs;
	// no org-role gate (e.g. GetMe, List/Create/Leave orgs).
	domainSelf
	// domainOrgRole: org-scoped; org_id comes from the request message and an
	// org-role gate applies (the RPCs migrated to RequirePermission, plus the
	// two whose gate lives in a read query / SQL CTE — see notes).
	domainOrgRole
	// domainProject: project-scoped via the x-project-id header. Project access
	// is established at auth time; the foundation applies NO further org-role
	// gate here (any org member, or a valid private API key, has full access —
	// exactly today's behavior). Tightening these is a later, deliberate phase.
	domainProject
	// domainSDKKey: API-key project scope (SDK, write-only).
	domainSDKKey
)

// permissionSpec is the authz decision for one RPC.
//
// For domainOrgRole entries, resource+action record the SEMANTIC permission the
// RPC represents. In this foundation the runtime gate is coarse — admin-gated
// org RPCs go through requireOrgAdmin (org:update) and member-gated ones through
// requireOrgMember/RequirePermission (org:read / project:read) — which yields
// outcomes identical to the prior hand-rolled checks, because admin holds every
// org/member/invitation/email_provider/project permission. Wiring per-RPC
// enforcement off these resource/action pairs is a later phase; the pairs are
// recorded now so that work is a policy/registry change, not rediscovery.
//
// No test cross-checks resource/action against the (resource, action) a handler
// actually passes to RequirePermission — they are documented INTENT, not a runtime
// assertion — because enforcement is per-handler, not driven from this table.
type permissionSpec struct {
	domain   authzDomain
	resource authz.Resource // set for domainOrgRole
	action   authz.Action   // set for domainOrgRole
	note     string
}

// orgRole builds a domainOrgRole permissionSpec — the only domain that carries a
// semantic (resource, action). Routing every org-role entry through orgRole keeps
// the "org-role ⟺ resource+action" invariant correct by construction: the other
// domains use bare struct literals and so cannot set resource/action by accident
// (TestPermissionRegistryOrgRoleEntriesAreComplete still backstops it). An
// optional note documents a coarse-today gate or a deliberate scoping choice.
func orgRole(resource authz.Resource, action authz.Action, note ...string) permissionSpec {
	s := permissionSpec{domain: domainOrgRole, resource: resource, action: action}
	if len(note) > 0 {
		s.note = note[0]
	}
	return s
}

// permissionRegistry maps every served RPC procedure to its authz decision.
// TestPermissionRegistryCoversAllProcedures asserts this map is exactly the set
// of served procedures (no missing entry — "no RPC ships without an authz
// decision" — and no stale entry), deriving the truth from the generated handler
// interfaces via reflection.
var permissionRegistry = map[string]permissionSpec{
	// --- public.auth.v1.AuthService — no auth ---
	"/public.auth.v1.AuthService/SignInWithEmail":     {domain: domainPublic},
	"/public.auth.v1.AuthService/RequestMagicLink":    {domain: domainPublic},
	"/public.auth.v1.AuthService/CompleteMagicLink":   {domain: domainPublic, note: "invite acceptance is authorized by invite-token possession, not an org role"},
	"/public.auth.v1.AuthService/CompleteOAuthSignIn": {domain: domainPublic},
	"/public.auth.v1.AuthService/RefreshSession":      {domain: domainPublic, note: "runs after access-token expiry; authorized by refresh-token possession"},
	"/public.auth.v1.AuthService/SignOut":             {domain: domainPublic},

	// --- public.dashboards.v1.SharedDashboardsService — no auth (share token) ---
	"/public.dashboards.v1.SharedDashboardsService/Query": {domain: domainPublic, note: "authorized by share_id"},

	// --- dashboard.orgs.v1.OrgsService ---
	"/dashboard.orgs.v1.OrgsService/List":              {domain: domainSelf, note: "returns only the caller's orgs"},
	"/dashboard.orgs.v1.OrgsService/Create":            {domain: domainSelf, note: "any authenticated customer may create an org"},
	"/dashboard.orgs.v1.OrgsService/Leave":             {domain: domainSelf, note: "self-service; last-admin/last-member guards live in the service"},
	"/dashboard.orgs.v1.OrgsService/Get":               orgRole(authz.ResourceOrg, authz.ActionRead, "membership enforced via the read query (NotFound to hide existence)"),
	"/dashboard.orgs.v1.OrgsService/ListMembers":       orgRole(authz.ResourceMember, authz.ActionRead),
	"/dashboard.orgs.v1.OrgsService/UpdateDisplayName": orgRole(authz.ResourceOrg, authz.ActionUpdate),
	"/dashboard.orgs.v1.OrgsService/InviteMember":      orgRole(authz.ResourceInvitation, authz.ActionCreate),
	"/dashboard.orgs.v1.OrgsService/ResendInvite":      orgRole(authz.ResourceInvitation, authz.ActionUpdate),
	"/dashboard.orgs.v1.OrgsService/ListInvitations":   orgRole(authz.ResourceInvitation, authz.ActionRead),
	"/dashboard.orgs.v1.OrgsService/RemoveMember":      orgRole(authz.ResourceMember, authz.ActionDelete),
	"/dashboard.orgs.v1.OrgsService/UpdateMemberRole":  orgRole(authz.ResourceMember, authz.ActionUpdate),

	// --- dashboard.projects.v1.ProjectsService ---
	"/dashboard.projects.v1.ProjectsService/BatchGet":             orgRole(authz.ResourceProject, authz.ActionRead),
	"/dashboard.projects.v1.ProjectsService/Create":               orgRole(authz.ResourceProject, authz.ActionCreate, "admin gate enforced in the CreateProjectAsAdmin SQL CTE"),
	"/dashboard.projects.v1.ProjectsService/Get":                  {domain: domainProject},
	"/dashboard.projects.v1.ProjectsService/Delete":               {domain: domainProject},
	"/dashboard.projects.v1.ProjectsService/UpdateMeta":           {domain: domainProject},
	"/dashboard.projects.v1.ProjectsService/UpdateFCMServiceJSON": {domain: domainProject},

	// --- dashboard.dashboards.v1.DashboardsService — project-scoped (JWT + x-project-id) ---
	"/dashboard.dashboards.v1.DashboardsService/Create":         {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/Get":            {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/List":           {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/Update":         {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/Delete":         {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/Upsert":         {domain: domainProject},
	"/dashboard.dashboards.v1.DashboardsService/QueryDashboard": {domain: domainProject},

	// --- dashboard.orgemailproviders.v1.OrgEmailProvidersService — admin-gated ---
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Get":      orgRole(authz.ResourceEmailProvider, authz.ActionRead),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Set":      orgRole(authz.ResourceEmailProvider, authz.ActionUpdate),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Remove":   orgRole(authz.ResourceEmailProvider, authz.ActionDelete),
	"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/SendTest": orgRole(authz.ResourceEmailProvider, authz.ActionUpdate),

	// --- dashboard.customers.v1.CustomersService — self-service ---
	"/dashboard.customers.v1.CustomersService/GetMe":       {domain: domainSelf},
	"/dashboard.customers.v1.CustomersService/SetPassword": {domain: domainSelf},

	// --- shared.insights.v1.InsightsService — project-scoped (JWT or private key) ---
	"/shared.insights.v1.InsightsService/Query":             {domain: domainProject},
	"/shared.insights.v1.InsightsService/SegmentUsers":      {domain: domainProject},
	"/shared.insights.v1.InsightsService/GetFilterSchema":   {domain: domainProject},
	"/shared.insights.v1.InsightsService/GetPropertyValues": {domain: domainProject},

	// --- shared.activity.v1.ActivityService — project-scoped ---
	"/shared.activity.v1.ActivityService/GetActivityFeed":    {domain: domainProject},
	"/shared.activity.v1.ActivityService/GetEventExplorer":   {domain: domainProject},
	"/shared.activity.v1.ActivityService/GetFilterSchema":    {domain: domainProject},
	"/shared.activity.v1.ActivityService/GetPropertyValues":  {domain: domainProject},
	"/shared.activity.v1.ActivityService/GetActivityHeatmap": {domain: domainProject},
	"/shared.activity.v1.ActivityService/GetProfileStats":    {domain: domainProject},

	// --- shared.profiles.v1.ProfilesService — project-scoped ---
	"/shared.profiles.v1.ProfilesService/Get":                {domain: domainProject},
	"/shared.profiles.v1.ProfilesService/GetByExternalId":    {domain: domainProject},
	"/shared.profiles.v1.ProfilesService/List":               {domain: domainProject},
	"/shared.profiles.v1.ProfilesService/Delete":             {domain: domainProject},
	"/shared.profiles.v1.ProfilesService/DeleteDataSubject":  {domain: domainProject, note: "GDPR/DPDP erasure; project-scoped today (member or private key). Tightening to admin is a later phase."},
	"/shared.profiles.v1.ProfilesService/GetDeletionRequest": {domain: domainProject},

	// --- sdk.profiles.v1.ProfilesSDKService — API key ---
	"/sdk.profiles.v1.ProfilesSDKService/Identify": {domain: domainSDKKey},

	// --- sdk.events.v1.EventsService — API key ---
	"/sdk.events.v1.EventsService/BatchCreate": {domain: domainSDKKey},
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
