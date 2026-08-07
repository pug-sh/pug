package authz

// modelText is the Casbin model: RBAC with a role hierarchy and no domains.
//
//   - g(r.sub, p.sub): role hierarchy — a higher role inherits a lower role's
//     permissions (admin inherits member). g is used ONLY for role->role links,
//     never per-user, so nothing here depends on the database.
//   - keyMatch(r.obj, p.obj): exact resource match today; also lets a future
//     super-role use "*" as a resource wildcard without changing the model.
//   - r.act == p.act: a plain action match. The "manage" umbrella is expanded
//     into explicit CRUD rules at load time (see manage()), so the matcher stays
//     trivial and the specials (send/export/erase) are never implied.
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

// Role identifiers used as Casbin subjects. These MUST equal the values stored
// in org_members.role and the orgs.Role consts — a drift test (policy_test.go)
// asserts every role used in the policy parses via orgs.ParseRole, so a role
// resolved from the DB can be passed straight into Authorize with no translation.
const (
	roleAdmin  = "ORG_ROLE_ADMIN"
	roleMember = "ORG_ROLE_MEMBER"
	roleViewer = "ORG_ROLE_VIEWER"

	// Reserved for the roadmap but NOT activated: the proto OrgRole enum and the
	// org_members check constraint do not permit this value, so no row can carry
	// it and no policy grants it. Activating one means: add it here, grant it
	// below, extend the hierarchy, and add the proto enum value + DB constraint
	// value (the recipe viewer followed).
	//   roleOwner = "ORG_ROLE_OWNER" // inherits admin; + org:delete, billing:manage
)

// crudActions are the actions the "manage" umbrella grants — the only actions
// today. Any future non-CRUD action must be granted explicitly, never added
// here — see Action docs.
var crudActions = []Action{ActionCreate, ActionRead, ActionUpdate, ActionDelete}

// manage expands full control over each resource into explicit CRUD rules.
// Authoring sugar: the matrix reads as "member manages campaigns" while the
// enforcer matcher stays a trivial r.act == p.act.
func manage(role string, resources ...Resource) [][]string {
	rules := make([][]string, 0, len(resources)*len(crudActions))
	for _, res := range resources {
		for _, act := range crudActions {
			rules = append(rules, []string{role, string(res), string(act)})
		}
	}
	return rules
}

// grant builds explicit (role, resource, action) rules — for read-only or
// special (non-CRUD) grants.
func grant(role string, res Resource, acts ...Action) [][]string {
	rules := make([][]string, 0, len(acts))
	for _, act := range acts {
		rules = append(rules, []string{role, string(res), string(act)})
	}
	return rules
}

// groupingRules define the role hierarchy (g), most-privileged first: admin
// inherits member, member inherits viewer. Admin therefore transitively holds
// every member and viewer grant, and member holds every viewer grant.
// Roadmap: owner -> admin when that role is activated.
var groupingRules = [][]string{
	{roleAdmin, roleMember},
	{roleMember, roleViewer},
}

// policyRules is the role -> permission matrix. Roles nest as
// viewer ⊂ member ⊂ admin via groupingRules, so each block grants only what that
// role ADDS over the one it inherits:
//
//   - viewer: read-only floor — every read an org member has (the project-scoped
//     data plus the org-level view: the org, the member list, the project list).
//   - member: full CRUD on project-scoped resources, on top of the viewer floor
//     it inherits (current reality: any org member has full access to project
//     data + a read-only org view).
//   - admin: org administration (org/member/invitation/email_provider/project)
//     on top of everything a member can do (inherited via groupingRules).
//
// member and admin keep the exact effective permissions they had before viewer
// existed — viewer only factors the shared reads into a named floor. Role
// assignment lives in Postgres, not here. Adding a permission is a one-line edit;
// it is reviewed like code.
var policyRules = buildPolicyRules()

func buildPolicyRules() [][]string {
	var rules [][]string

	// viewer — read-only floor inherited by member (and transitively admin):
	// read on every project-scoped resource plus the org-level entities any
	// member can see (view org, list members/projects).
	rules = append(rules, grant(roleViewer, ResourceDashboard, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceInsight, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceActivity, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceProfile, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceOrg, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceMember, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceProject, ActionRead)...)
	rules = append(rules, grant(roleViewer, ResourceAPIKey, ActionRead)...)

	// member — full CRUD on all project-scoped resources. The read half overlaps
	// the viewer floor member inherits; manage() stays readable and the overlap
	// is inert.
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

	// admin — minting and revoking a project's API keys. Granted explicitly
	// rather than via manage(): there is no update action to confer, and the read
	// half is already the viewer floor above.
	rules = append(rules, grant(roleAdmin, ResourceAPIKey, ActionCreate, ActionDelete)...)

	return rules
}
