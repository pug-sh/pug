// Package authz centralizes pug's role-based authorization policy.
//
// It holds the role -> permission matrix and the role hierarchy as an in-memory
// Casbin enforcer. Role ASSIGNMENT (who has which role in which org) stays in
// Postgres (org_members) and is resolved fresh per request by the caller, who
// passes the resolved role into Authorize. Casbin therefore answers only one
// question: "may this role perform this action on this resource?" — there is no
// Casbin<->DB sync and no distributed cache invalidation to get wrong.
//
// The model and policy are plain Go (no .conf / .csv / go:embed): both are
// reviewed like code and change only on deploy.
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
	// Org-scoped resources, each gating on its own resource. project's Create
	// additionally gates race-safe in SQL.
	ResourceOrg           Resource = "org"
	ResourceMember        Resource = "member"
	ResourceInvitation    Resource = "invitation"
	ResourceEmailProvider Resource = "email_provider"
	ResourceProject       Resource = "project"

	// ResourceUsage is an org's metered event counts. Read-only and on the viewer
	// floor: it spans every project the org owns, and the person who notices a
	// spike is rarely the admin. The meter writes it, no RPC does.
	ResourceUsage Resource = "usage"

	// ResourceBilling is an org's entitlement — its plan, quota and period — and the
	// checkout and portal sessions that buy one. Reads are on the viewer floor beside
	// ResourceUsage; create is admin-only and mints a session, never an entitlement.
	ResourceBilling Resource = "billing"

	// ResourceInvoice is an org's invoice ledger. Read-only and admin-only: a
	// receipt carries a company's billing details, unlike the priced estimate,
	// which stays on the billing floor.
	ResourceInvoice Resource = "invoice"

	// ResourceAPIKey is a project's API keys, so its org resolves from the
	// x-project-id project like the project-data resources below. Minting a
	// credential for a whole project is an administrative act, so create/delete sit
	// with admin while every role reads. There is deliberately no update action —
	// a key is created and revoked, never edited.
	ResourceAPIKey Resource = "api_key"

	// Project-data resources, their org resolved from the x-project-id project.
	// viewer holds read on each, member full CRUD. The API-key path is a deliberate
	// no-op (coarse project scope).
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

// allResources and allActions are the declared taxonomy the policy tests iterate.
// Every declared resource must be granted to at least one role: an ungranted one
// would make every check against it fail closed but SILENTLY, since Casbin simply
// never matches. Keep these in sync when adding a const.
var allResources = []Resource{
	ResourceOrg, ResourceMember, ResourceInvitation, ResourceEmailProvider,
	ResourceProject, ResourceUsage, ResourceBilling, ResourceInvoice, ResourceAPIKey,
	ResourceDashboard, ResourceInsight, ResourceActivity, ResourceProfile,
}

var allActions = []Action{ActionCreate, ActionRead, ActionUpdate, ActionDelete}
