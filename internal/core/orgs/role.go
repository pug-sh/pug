package orgs

// Role is the canonical org-role identifier used at the service boundary.
// String values match what is stored in org_members.role and what the proto
// OrgRole enum's String() form yields, so DB rows, service code, and wire
// values map 1:1 with no proto import required inside this package.
type Role string

const (
	RoleAdmin  Role = "ORG_ROLE_ADMIN"
	RoleMember Role = "ORG_ROLE_MEMBER"
)

func (r Role) String() string { return string(r) }
