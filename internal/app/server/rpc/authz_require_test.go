package rpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

// fakeRoleLookup satisfies memberRoleLookup with a canned role/error, so
// requirePermission can be exercised with no DB or Redis.
type fakeRoleLookup struct {
	role coreorgs.Role
	err  error
}

func (f fakeRoleLookup) GetMemberRole(context.Context, string, string) (coreorgs.Role, error) {
	return f.role, f.err
}

func customerCtx() context.Context {
	return authn.SetInfo(context.Background(), &Principal{
		AuthType: AuthTypeJWT,
		Customer: &dbread.Customer{ID: "cust-1"},
	})
}

// TestRequirePermission exercises the core authorization check against the real
// authz policy, covering each branch with a faked role lookup (no infra).
func TestRequirePermission(t *testing.T) {
	authorizer, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	t.Run("no customer principal is unauthenticated", func(t *testing.T) {
		// No authn info on the context → MustGetPrincipalWithCustomer fails before
		// any role lookup.
		_, err := requirePermission(context.Background(), authorizer, fakeRoleLookup{role: coreorgs.RoleAdmin},
			"org-1", authz.ResourceOrg, authz.ActionUpdate)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
			t.Fatalf("want Unauthenticated *apperr.Error, got %v", err)
		}
	})

	t.Run("non-member is permission denied (ORG_NOT_A_MEMBER)", func(t *testing.T) {
		_, err := requirePermission(customerCtx(), authorizer, fakeRoleLookup{err: coreorgs.ErrMemberNotFound},
			"org-1", authz.ResourceOrg, authz.ActionUpdate)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code() != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied *apperr.Error, got %v", err)
		}
		if ae.Reason() != apperr.ReasonOrgNotAMember {
			t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgNotAMember)
		}
	})

	t.Run("role-lookup failure is internal", func(t *testing.T) {
		_, err := requirePermission(customerCtx(), authorizer, fakeRoleLookup{err: errors.New("db down")},
			"org-1", authz.ResourceOrg, authz.ActionUpdate)
		if got := connect.CodeOf(err); got != connect.CodeInternal {
			t.Fatalf("code = %v, want CodeInternal (err=%v)", got, err)
		}
	})

	t.Run("member denied an admin-only action is ORG_ROLE_FORBIDDEN", func(t *testing.T) {
		_, err := requirePermission(customerCtx(), authorizer, fakeRoleLookup{role: coreorgs.RoleMember},
			"org-1", authz.ResourceOrg, authz.ActionUpdate)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code() != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied *apperr.Error, got %v", err)
		}
		// Lacks-permission is uniform ORG_ROLE_FORBIDDEN (distinct from the
		// non-member ORG_NOT_A_MEMBER above).
		if ae.Reason() != apperr.ReasonOrgRoleForbidden {
			t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgRoleForbidden)
		}
	})

	t.Run("member allowed a member action returns the principal", func(t *testing.T) {
		p, err := requirePermission(customerCtx(), authorizer, fakeRoleLookup{role: coreorgs.RoleMember},
			"org-1", authz.ResourceOrg, authz.ActionRead)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil || p.Customer == nil || p.Customer.ID != "cust-1" {
			t.Fatalf("principal = %+v, want customer cust-1", p)
		}
	})

	t.Run("admin allowed an admin action returns the principal", func(t *testing.T) {
		p, err := requirePermission(customerCtx(), authorizer, fakeRoleLookup{role: coreorgs.RoleAdmin},
			"org-1", authz.ResourceOrg, authz.ActionUpdate)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil || p.Customer == nil || p.Customer.ID != "cust-1" {
			t.Fatalf("principal = %+v, want customer cust-1", p)
		}
	})
}
