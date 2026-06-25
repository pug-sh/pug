package rpc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// memberRoleLookup resolves a customer's role within an org. It is satisfied by
// *coreorgs.Service (whose GetMemberRole is Redis-cached + drift-validated).
// Declared as an interface so the rpc package depends on the capability rather
// than the concrete service, mirroring projectKeyLookup in auth.go.
type memberRoleLookup interface {
	GetMemberRole(ctx context.Context, orgID, customerID string) (coreorgs.Role, error)
}

// RequirePermission is the single authorization entry point for dashboard
// (JWT/customer) handlers. It (1) requires a customer principal, (2) resolves
// that customer's role in orgID fresh from the DB, and (3) asks the shared authz
// policy whether that role may perform action on resource.
//
// Role assignment is resolved per request (roles are deliberately NOT in the
// JWT); the policy comes from the injected Authorizer. On failure it returns the
// SAME typed apperr the hand-rolled requireOrgAdmin/requireOrgMember helpers
// returned, so the migration is behavior-preserving:
//
//   - not a member            -> PermissionDenied(ORG_NOT_A_MEMBER)
//   - member lacking the perm -> PermissionDenied(deniedReason, deniedMsg)
//   - role-lookup / enforce failure -> CodeInternal
func RequirePermission(
	ctx context.Context,
	authorizer *authz.Authorizer,
	lookup memberRoleLookup,
	orgID string,
	resource authz.Resource,
	action authz.Action,
	deniedReason apperr.Reason,
	deniedMsg string,
) (*Principal, error) {
	principal, err := MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	role, err := lookup.GetMemberRole(ctx, orgID, principal.Customer.ID)
	if err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			// Non-membership is always ORG_NOT_A_MEMBER, regardless of the
			// caller's deniedReason/deniedMsg (those describe the lacks-permission
			// case below) — matching every prior hand-rolled check.
			return nil, apperr.PermissionDenied(apperr.ReasonOrgNotAMember, "not a member of this org")
		}
		// The lookup logs + records non-sentinel failures at source.
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	ok, err := authorizer.Authorize(role.String(), resource, action)
	if err != nil {
		slog.ErrorContext(ctx, "authorization check failed", slogx.Error(err),
			slog.String("org_id", orgID),
			slog.String("resource", string(resource)),
			slog.String("action", string(action)))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !ok {
		return nil, apperr.PermissionDenied(deniedReason, deniedMsg)
	}

	return principal, nil
}
