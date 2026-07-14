package authzspec

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/authz"
)

// TestGatedBuildersSetFullTriple pins that the two role-gated constructors set
// IsRoleGated + the (resource, action, orgSource) triple the interceptor reads —
// this is the runtime half of the compile-enforced invariant.
func TestGatedBuildersSetFullTriple(t *testing.T) {
	t.Run("OrgGated resolves from the message", func(t *testing.T) {
		s := OrgGated(authz.ResourceInvitation, authz.ActionCreate, "note")
		if !s.IsRoleGated() {
			t.Fatal("OrgGated must be role-gated")
		}
		if s.Resource() != authz.ResourceInvitation || s.Action() != authz.ActionCreate {
			t.Errorf("triple = (%q, %q), want (invitation, create)", s.Resource(), s.Action())
		}
		if s.OrgSource() != OrgFromMessage {
			t.Errorf("orgSource = %v, want message", s.OrgSource())
		}
		if !s.Defined() {
			t.Error("OrgGated must be Defined")
		}
		if s.Note() != "note" {
			t.Errorf("note = %q, want \"note\"", s.Note())
		}
	})

	t.Run("ProjGated resolves from the project", func(t *testing.T) {
		s := ProjGated(authz.ResourceDashboard, authz.ActionDelete)
		if !s.IsRoleGated() || s.OrgSource() != OrgFromProject {
			t.Fatalf("ProjGated must be role-gated with project orgSource, got gated=%v src=%v", s.IsRoleGated(), s.OrgSource())
		}
		if s.Resource() != authz.ResourceDashboard || s.Action() != authz.ActionDelete {
			t.Errorf("triple = (%q, %q), want (dashboard, delete)", s.Resource(), s.Action())
		}
	})
}

// TestNonGatedBuildersCarryNoTriple pins that the non-gated constructors are
// Defined but never role-gated and never carry a (resource, action, orgSource) —
// the property the unexported fields make impossible to violate from outside.
func TestNonGatedBuildersCarryNoTriple(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"Public", Public()},
		{"Self", Self()},
		{"Project", Project()},
		{"SDKKey", SDKKey()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.spec.Defined() {
				t.Error("must be Defined")
			}
			if tc.spec.IsRoleGated() {
				t.Error("must not be role-gated")
			}
			if tc.spec.Resource() != "" || tc.spec.Action() != "" {
				t.Errorf("must carry no resource/action, got (%q, %q)", tc.spec.Resource(), tc.spec.Action())
			}
			if tc.spec.OrgSource() != 0 {
				t.Errorf("must carry no orgSource, got %v", tc.spec.OrgSource())
			}
		})
	}
}

// TestZeroSpecIsNotDefined pins that the one literal expressible outside this
// package, Spec{}, is rejected by Defined() (and is not role-gated) — closing the
// bare-literal residual the registry-completeness test guards against.
func TestZeroSpecIsNotDefined(t *testing.T) {
	var s Spec
	if s.Defined() {
		t.Error("zero Spec{} must not be Defined")
	}
	if s.IsRoleGated() {
		t.Error("zero Spec{} must not be role-gated")
	}
}
