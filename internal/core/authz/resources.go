// Package authz centralizes pug's role-based authorization policy.
//
// It holds the role -> permission matrix and the role hierarchy as an in-memory
// Casbin enforcer. Role ASSIGNMENT (who has which role in which org) stays in
// Postgres (org_members) and is resolved fresh per request by the caller, who
// passes the resolved role(s) into Authorize. Casbin therefore answers only one
// question: "may this role perform this action on this resource?" — there is no
// Casbin<->DB sync and no distributed cache invalidation to get wrong.
//
// The model and policy are plain Go (no .conf / .csv / go:embed): the model is
// the static grammar and the policy is the role->permission matrix, both
// reviewed like code and changed only on deploy. Adding a permission is a
// one-line edit in policy.go; adding a role is a one-line edit there plus the
// assignment plumbing (proto enum + org_members check constraint).
package authz

// Resource identifies a protected resource type — the Casbin object. A resource
// is declared here only when a real served RPC needs it; speculative/roadmap
// resources are intentionally absent (add them when the feature actually lands).
type Resource string

// Action identifies an operation on a resource — the Casbin action.
//
// Today the only actions are the four CRUD verbs, granted in bulk by the "manage"
// authoring helper (see policy.go). Any future non-CRUD action (e.g. erase for
// GDPR/DPDP, export, send) must be added here and granted explicitly — never
// folded into manage — so "manage X" can never silently confer it.
type Action string

const (
	// Org-scoped resources, all backing real dashboard org/admin RPCs. Each is
	// enforced by AuthzInterceptor from the (resource, action) recorded in
	// authz_registry.go — org/member/invitation/email_provider/project/usage all
	// gate on their own resource (project's Create additionally gates race-safe in
	// SQL).
	ResourceOrg           Resource = "org"
	ResourceMember        Resource = "member"
	ResourceInvitation    Resource = "invitation"
	ResourceEmailProvider Resource = "email_provider"
	ResourceProject       Resource = "project"

	// ResourceUsage is an org's metered event counts. Read-only and on the viewer
	// floor: it is a reporting number spanning every project the org owns, and the
	// person who notices a spike is rarely the admin. There is nothing to create,
	// update or delete — the meter writes it, no RPC does.
	ResourceUsage Resource = "usage"

	// ResourceBilling is an org's entitlement — its plan and event quota.
	// Read-only and on the viewer floor, for the same reason as ResourceUsage:
	// it is the other half of "X of Y", and a member who notices the limit is
	// rarely the admin. There is nothing to create, update or delete because no
	// RPC changes an entitlement — `pug billing` does, at the operator's trust
	// level. Money moves will need their own actions when checkout lands.
	ResourceBilling Resource = "billing"

	// ResourceAPIKey is a project's API keys. Its org is resolved from the
	// x-project-id project like the project-data resources below, but minting a
	// credential for a whole project is an administrative act, so create/delete
	// sit with admin rather than member. Every role reads (a key list is what the
	// project settings page shows), and there is deliberately no update action —
	// a key is created and revoked, never edited.
	ResourceAPIKey Resource = "api_key"

	// Project-data resources, enforced by AuthzInterceptor from the (resource,
	// action) recorded in authz_registry.go's projGated entries (org resolved from
	// the x-project-id project). viewer holds read on each (the read-only floor);
	// member holds full CRUD. The API-key path is a deliberate no-op (coarse
	// project scope).
	ResourceDashboard Resource = "dashboard"
	ResourceInsight   Resource = "insight"
	ResourceActivity  Resource = "activity"
	ResourceProfile   Resource = "profile"
)

const (
	ActionCreate Action = "create"
	ActionRead   Action = "read"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// allResources and allActions are the declared taxonomy — the single source of
// truth the policy tests iterate. Every declared resource must be granted to at
// least one role: a declared-but-ungranted resource would make every check
// against it fail closed but SILENTLY (Casbin simply never matches), so
// policy_test.go asserts full coverage. Keep these in sync when adding a const.
var allResources = []Resource{
	ResourceOrg, ResourceMember, ResourceInvitation, ResourceEmailProvider,
	ResourceProject, ResourceUsage, ResourceBilling, ResourceAPIKey,
	ResourceDashboard, ResourceInsight, ResourceActivity, ResourceProfile,
}

var allActions = []Action{ActionCreate, ActionRead, ActionUpdate, ActionDelete}
