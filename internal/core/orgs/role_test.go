package orgs_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
)

// TestRoleStringsMatchProtoEnum pins the load-bearing invariant from role.go's
// doc comment: the string form of each coreorgs.Role constant is identical to
// the proto OrgRole enum's String() form. The conversion in toRPCRole assumes
// this 1:1 mapping; a proto enum rename would otherwise be a silent break
// caught only by integration tests.
func TestRoleStringsMatchProtoEnum(t *testing.T) {
	cases := []struct {
		role  orgs.Role
		proto orgsv1.OrgRole
	}{
		{orgs.RoleAdmin, orgsv1.OrgRole_ORG_ROLE_ADMIN},
		{orgs.RoleMember, orgsv1.OrgRole_ORG_ROLE_MEMBER},
	}
	for _, c := range cases {
		if got, want := c.role.String(), c.proto.String(); got != want {
			t.Errorf("role %v: got %q, want %q (proto enum)", c.role, got, want)
		}
	}
}

// TestParseRoleAcceptsKnownValues confirms the constructor accepts every
// canonical role and rejects anything else, including the empty string.
func TestParseRoleAcceptsKnownValues(t *testing.T) {
	for _, s := range []string{"ORG_ROLE_ADMIN", "ORG_ROLE_MEMBER"} {
		r, err := orgs.ParseRole(s)
		if err != nil {
			t.Errorf("ParseRole(%q): unexpected error %v", s, err)
		}
		if r.String() != s {
			t.Errorf("ParseRole(%q): got %q", s, r.String())
		}
	}
}

func TestParseRoleRejectsUnknown(t *testing.T) {
	for _, s := range []string{"", "ADMIN", "ORG_ROLE_OWNER", "garbage"} {
		if _, err := orgs.ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q): want error, got nil", s)
		}
	}
}

func TestRoleIsValid(t *testing.T) {
	if !orgs.RoleAdmin.IsValid() {
		t.Error("RoleAdmin should be valid")
	}
	if !orgs.RoleMember.IsValid() {
		t.Error("RoleMember should be valid")
	}
	if orgs.Role("").IsValid() {
		t.Error("empty Role should NOT be valid")
	}
	if orgs.Role("ORG_ROLE_OWNER").IsValid() {
		t.Error("ORG_ROLE_OWNER should NOT be valid")
	}
}
