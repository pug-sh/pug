package authz

import "testing"

func newTestAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	a, err := NewAuthorizer()
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	return a
}

func TestNewAuthorizer(t *testing.T) {
	if _, err := NewAuthorizer(); err != nil {
		t.Fatalf("NewAuthorizer returned error for the static policy: %v", err)
	}
}

// TestAuthorizeMatrix is the spec of "who can do what". viewer = read-only floor
// (project-scoped reads + read-only org view); member = full CRUD on
// project-scoped resources on top of that floor (inherited); admin = org
// administration on top of member (inherited). member/admin keep the exact
// effective permissions they had before viewer was introduced.
func TestAuthorizeMatrix(t *testing.T) {
	a := newTestAuthorizer(t)

	cases := []struct {
		name     string
		role     string
		resource Resource
		action   Action
		want     bool
	}{
		// member — full CRUD on project-scoped resources.
		{"member dashboard read", roleMember, ResourceDashboard, ActionRead, true},
		{"member dashboard create", roleMember, ResourceDashboard, ActionCreate, true},
		{"member dashboard update", roleMember, ResourceDashboard, ActionUpdate, true},
		{"member dashboard delete", roleMember, ResourceDashboard, ActionDelete, true},
		{"member insight read", roleMember, ResourceInsight, ActionRead, true},
		{"member activity read", roleMember, ResourceActivity, ActionRead, true},
		{"member profile delete", roleMember, ResourceProfile, ActionDelete, true},

		// member — read-only on org-level entities.
		{"member org read", roleMember, ResourceOrg, ActionRead, true},
		{"member member read", roleMember, ResourceMember, ActionRead, true},
		{"member project read", roleMember, ResourceProject, ActionRead, true},

		// member — denied org administration.
		{"member org update", roleMember, ResourceOrg, ActionUpdate, false},
		{"member org delete", roleMember, ResourceOrg, ActionDelete, false},
		{"member org create", roleMember, ResourceOrg, ActionCreate, false},
		{"member member update", roleMember, ResourceMember, ActionUpdate, false},
		{"member member delete", roleMember, ResourceMember, ActionDelete, false},
		{"member invitation read", roleMember, ResourceInvitation, ActionRead, false},
		{"member invitation create", roleMember, ResourceInvitation, ActionCreate, false},
		{"member email_provider read", roleMember, ResourceEmailProvider, ActionRead, false},
		{"member project create", roleMember, ResourceProject, ActionCreate, false},
		{"member project delete", roleMember, ResourceProject, ActionDelete, false},

		// admin — org administration.
		{"admin org update", roleAdmin, ResourceOrg, ActionUpdate, true},
		{"admin org delete", roleAdmin, ResourceOrg, ActionDelete, true},
		{"admin org create", roleAdmin, ResourceOrg, ActionCreate, true},
		{"admin org read", roleAdmin, ResourceOrg, ActionRead, true},
		{"admin member delete", roleAdmin, ResourceMember, ActionDelete, true},
		{"admin member update", roleAdmin, ResourceMember, ActionUpdate, true},
		{"admin invitation create", roleAdmin, ResourceInvitation, ActionCreate, true},
		{"admin invitation read", roleAdmin, ResourceInvitation, ActionRead, true},
		{"admin email_provider read", roleAdmin, ResourceEmailProvider, ActionRead, true},
		{"admin email_provider update", roleAdmin, ResourceEmailProvider, ActionUpdate, true},
		{"admin email_provider delete", roleAdmin, ResourceEmailProvider, ActionDelete, true},
		{"admin project create", roleAdmin, ResourceProject, ActionCreate, true},
		{"admin project delete", roleAdmin, ResourceProject, ActionDelete, true},

		// admin — inherits member's project-scoped CRUD via the role hierarchy.
		{"admin dashboard read", roleAdmin, ResourceDashboard, ActionRead, true},
		{"admin dashboard delete", roleAdmin, ResourceDashboard, ActionDelete, true},
		{"admin profile delete", roleAdmin, ResourceProfile, ActionDelete, true},

		// viewer — read-only floor: project-scoped reads + the org-level view.
		{"viewer dashboard read", roleViewer, ResourceDashboard, ActionRead, true},
		{"viewer insight read", roleViewer, ResourceInsight, ActionRead, true},
		{"viewer activity read", roleViewer, ResourceActivity, ActionRead, true},
		{"viewer profile read", roleViewer, ResourceProfile, ActionRead, true},
		{"viewer org read", roleViewer, ResourceOrg, ActionRead, true},
		{"viewer member read", roleViewer, ResourceMember, ActionRead, true},
		{"viewer project read", roleViewer, ResourceProject, ActionRead, true},

		// viewer — denied every write, plus the admin-only org reads.
		{"viewer dashboard create", roleViewer, ResourceDashboard, ActionCreate, false},
		{"viewer dashboard update", roleViewer, ResourceDashboard, ActionUpdate, false},
		{"viewer dashboard delete", roleViewer, ResourceDashboard, ActionDelete, false},
		{"viewer profile delete", roleViewer, ResourceProfile, ActionDelete, false},
		{"viewer project create", roleViewer, ResourceProject, ActionCreate, false},
		{"viewer project delete", roleViewer, ResourceProject, ActionDelete, false},
		{"viewer org update", roleViewer, ResourceOrg, ActionUpdate, false},
		{"viewer invitation read", roleViewer, ResourceInvitation, ActionRead, false},
		{"viewer email_provider read", roleViewer, ResourceEmailProvider, ActionRead, false},

		// unknown role gets nothing.
		{"unknown role dashboard read", "ORG_ROLE_BOGUS", ResourceDashboard, ActionRead, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.Authorize(tc.role, tc.resource, tc.action)
			if err != nil {
				t.Fatalf("Authorize(%q,%q,%q): unexpected error: %v", tc.role, tc.resource, tc.action, err)
			}
			if got != tc.want {
				t.Errorf("Authorize(%q,%q,%q) = %v, want %v", tc.role, tc.resource, tc.action, got, tc.want)
			}
		})
	}
}

func TestAuthorizeEmptyRoleDenies(t *testing.T) {
	a := newTestAuthorizer(t)
	got, err := a.Authorize("", ResourceDashboard, ActionRead)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got {
		t.Error("Authorize with an empty role = true, want false")
	}
}

// TestManageDoesNotConferSpecialActions locks in the resources.go contract that
// the "manage" umbrella grants ONLY CRUD: a future non-CRUD action (erase,
// export, send) must be granted explicitly and stays denied until then — even to
// a role that "manages" the resource. Raw Action stand-ins pin the invariant now,
// before any such action is declared.
func TestManageDoesNotConferSpecialActions(t *testing.T) {
	a := newTestAuthorizer(t)
	// admin "manages" org (full CRUD via the policy); non-CRUD actions must still
	// be denied.
	for _, act := range []Action{"erase", "export", "send"} {
		got, err := a.Authorize(roleAdmin, ResourceOrg, act)
		if err != nil {
			t.Fatalf("Authorize(admin, org, %q): %v", act, err)
		}
		if got {
			t.Errorf("Authorize(admin, org, %q) = true; manage must not confer non-CRUD actions", act)
		}
	}
}
