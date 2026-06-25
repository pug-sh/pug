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
	// authz_registry.go — org/member/invitation/email_provider/project all gate on
	// their own resource (project's Create additionally gates race-safe in SQL).
	ResourceOrg           Resource = "org"
	ResourceMember        Resource = "member"
	ResourceInvitation    Resource = "invitation"
	ResourceEmailProvider Resource = "email_provider"
	ResourceProject       Resource = "project"

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
	ResourceProject, ResourceDashboard, ResourceInsight, ResourceActivity,
	ResourceProfile,
}

var allActions = []Action{ActionCreate, ActionRead, ActionUpdate, ActionDelete}
