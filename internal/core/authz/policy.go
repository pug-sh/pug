package authz

// modelText is the Casbin model: RBAC with a role hierarchy and no domains.
//
//   - g(r.sub, p.sub): role hierarchy — a higher role inherits a lower role's
//     permissions. Used ONLY for role->role links, never per-user, so nothing
//     here depends on the database.
//   - keyMatch(r.obj, p.obj): exact resource match today; also lets a future
//     super-role use "*" as a resource wildcard without changing the model.
//   - r.act == p.act: "manage" is expanded into explicit CRUD rules at load time
//     (see manage()), so the specials (send/export/erase) are never implied.
const modelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && keyMatch(r.obj, p.obj) && r.act == p.act
`

// Role identifiers used as Casbin subjects. These MUST equal the values stored in
// org_members.role and the orgs.Role consts, so a role resolved from the DB can be
// passed straight into Authorize; TestPolicyRoleStringsMatchOrgRoles pins that.
const (
	roleAdmin  = "ORG_ROLE_ADMIN"
	roleMember = "ORG_ROLE_MEMBER"
	roleViewer = "ORG_ROLE_VIEWER"

	// Reserved for the roadmap but NOT activated: neither the proto OrgRole enum
	// nor the org_members check constraint permits this value, so no row can carry
	// it. Activating one means adding it here, granting it below, extending the
	// hierarchy, and adding the enum + constraint value (the recipe viewer followed).
	//   roleOwner = "ORG_ROLE_OWNER" // inherits admin; + org:delete, billing:manage
)

// crudActions are the actions "manage" grants — the only actions today. A future
// non-CRUD action must be granted explicitly, never added here; see Action docs.
var crudActions = []Action{ActionCreate, ActionRead, ActionUpdate, ActionDelete}

// manage expands full control over each resource into explicit CRUD rules, so the
// matrix reads as "member manages campaigns" while the matcher stays r.act == p.act.
func manage(role string, resources ...Resource) [][]string {
	rules := make([][]string, 0, len(resources)*len(crudActions))
	for _, res := range resources {
		rules = append(rules, grant(role, res, crudActions...)...)
	}
	return rules
}

// read is manage's read-only counterpart, for a role whose grant over a resource
// is the read alone.
func read(role string, resources ...Resource) [][]string {
	rules := make([][]string, 0, len(resources))
	for _, res := range resources {
		rules = append(rules, grant(role, res, ActionRead)...)
	}
	return rules
}

// grant builds explicit (role, resource, action) rules — the primitive read and
// manage are built from, and what a partial grant like create+delete uses.
func grant(role string, res Resource, acts ...Action) [][]string {
	rules := make([][]string, 0, len(acts))
	for _, act := range acts {
		rules = append(rules, []string{role, string(res), string(act)})
	}
	return rules
}

// groupingRules define the role hierarchy (g), most-privileged first.
// Roadmap: owner -> admin when that role is activated.
var groupingRules = [][]string{
	{roleAdmin, roleMember},
	{roleMember, roleViewer},
}

// policyRules is the role -> permission matrix. Roles nest viewer ⊂ member ⊂ admin
// via groupingRules, so each block below grants only what that role ADDS over the
// one it inherits. Role assignment itself lives in Postgres, not here.
var policyRules = buildPolicyRules()

func buildPolicyRules() [][]string {
	var rules [][]string

	// viewer — the read-only floor member and admin inherit: every project-scoped
	// resource plus the org-level entities any member can see.
	rules = append(rules, read(roleViewer,
		ResourceDashboard,
		ResourceInsight,
		ResourceActivity,
		ResourceProfile,
		ResourceOrg,
		ResourceMember,
		ResourceProject,
		ResourceAPIKey,
		ResourceUsage,
		ResourceBilling,
	)...)

	// member — full CRUD on all project-scoped resources. The read half overlaps
	// the viewer floor member inherits; the overlap is inert.
	rules = append(rules, manage(roleMember,
		ResourceDashboard,
		ResourceInsight,
		ResourceActivity,
		ResourceProfile,
	)...)

	// admin — org administration (member perms inherited via groupingRules).
	rules = append(rules, manage(roleAdmin,
		ResourceOrg,
		ResourceMember,
		ResourceInvitation,
		ResourceEmailProvider,
		ResourceProject,
	)...)

	// admin — minting and revoking a project's API keys. Explicit rather than
	// manage(): there is no update action to confer, and read is already the floor.
	rules = append(rules, grant(roleAdmin, ResourceAPIKey, ActionCreate, ActionDelete)...)

	// admin — starting a checkout or opening the payments portal. Create rather than
	// a new verb: both mint a provider session. The READ half stays on the floor.
	rules = append(rules, grant(roleAdmin, ResourceBilling, ActionCreate)...)

	// admin — the invoice ledger, whose receipts carry billing details.
	rules = append(rules, grant(roleAdmin, ResourceInvoice, ActionRead)...)

	return rules
}
