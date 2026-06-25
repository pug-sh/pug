package authz

import (
	"testing"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
)

// TestPolicyRoleStringsMatchOrgRoles pins the load-bearing invariant that every
// role used in the Casbin policy is a real, recognized org role. Casbin subjects
// are raw strings here (to keep this package decoupled from the orgs domain), so
// this guards against a typo or drift between the policy and org_members.role /
// the orgs.Role consts. If this fails, a role in the policy could never match a
// role resolved from the DB — silently denying or (worse) a stale grant.
func TestPolicyRoleStringsMatchOrgRoles(t *testing.T) {
	seen := map[string]struct{}{}
	for _, rule := range policyRules {
		seen[rule[0]] = struct{}{}
	}
	for _, link := range groupingRules {
		seen[link[0]] = struct{}{}
		seen[link[1]] = struct{}{}
	}

	for role := range seen {
		if _, err := coreorgs.ParseRole(role); err != nil {
			t.Errorf("policy references role %q which is not a valid orgs.Role: %v", role, err)
		}
	}
}

// TestPolicyResourcesAndActionsAreKnown catches a policy rule authored with a
// resource/action outside the declared taxonomy (e.g. a future raw-string rule).
func TestPolicyResourcesAndActionsAreKnown(t *testing.T) {
	knownResources := map[string]struct{}{}
	for _, r := range []Resource{
		ResourceOrg, ResourceMember, ResourceInvitation, ResourceEmailProvider,
		ResourceProject, ResourceDashboard, ResourceInsight, ResourceActivity,
		ResourceProfile,
	} {
		knownResources[string(r)] = struct{}{}
	}
	knownActions := map[string]struct{}{}
	for _, act := range []Action{
		ActionCreate, ActionRead, ActionUpdate, ActionDelete,
	} {
		knownActions[string(act)] = struct{}{}
	}

	for _, rule := range policyRules {
		if _, ok := knownResources[rule[1]]; !ok {
			t.Errorf("policy rule %v uses unknown resource %q", rule, rule[1])
		}
		if _, ok := knownActions[rule[2]]; !ok {
			t.Errorf("policy rule %v uses unknown action %q", rule, rule[2])
		}
	}
}
