package orgs

import "fmt"

// Role is the canonical org-role identifier used at the service boundary.
// String values match what is stored in org_members.role and what the proto
// OrgRole enum's String() form yields, so DB rows, service code, and wire
// values map 1:1 with no proto import required inside this package.
//
// The zero value Role("") is deliberately invalid (IsValid returns false): it
// is the reserved drift/UNSPECIFIED sentinel — roleFromDBJoinRow returns it for
// an unrecognized stored value, and toRPCRole maps it to ORG_ROLE_UNSPECIFIED.
type Role string

const (
	RoleAdmin  Role = "ORG_ROLE_ADMIN"
	RoleMember Role = "ORG_ROLE_MEMBER"
	RoleViewer Role = "ORG_ROLE_VIEWER"
)

func (r Role) String() string { return string(r) }

// IsValid reports whether r is one of the recognized roles. Use at every
// boundary where a Role is constructed from an untrusted string (DB read,
// proto conversion) to keep Role("garbage") out of the rest of the system.
func (r Role) IsValid() bool {
	switch r {
	case RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

// ParseRole converts an arbitrary string into a Role, returning an error for
// values outside the recognized set. Use when reading roles from sources that
// could carry drifted values (raw DB strings, untrusted input).
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.IsValid() {
		return "", fmt.Errorf("orgs: unrecognized role %q", s)
	}
	return r, nil
}
