package instance

import (
	"testing"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
)

func TestRoleDefaultsOnlyForInvitations(t *testing.T) {
	invitationRole, err := requestedRole(orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED)
	if err != nil || invitationRole != coreorgs.RoleMember {
		t.Fatalf("invitation role = %q, %v", invitationRole, err)
	}
	if _, err := requiredRole(orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED); err == nil {
		t.Fatal("membership role change accepted an unspecified role")
	}
	memberRole, err := requiredRole(orgsv1.OrgRole_ORG_ROLE_ADMIN)
	if err != nil || memberRole != coreorgs.RoleAdmin {
		t.Fatalf("required role = %q, %v", memberRole, err)
	}
}
