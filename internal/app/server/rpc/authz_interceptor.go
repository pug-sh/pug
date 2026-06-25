package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/authz"
)

// AuthzInterceptor is the single authorization gate. For every domainRoleGated
// procedure in permissionRegistry it resolves the caller's org role and enforces
// the recorded (resource, action) against the shared authz policy. Every other
// procedure (public / self / SDK / domainProject) passes through untouched.
//
// There is no per-handler authorization: handlers assume the request reaching
// them is already authorized. Because TestPermissionRegistryCoversAllProcedures
// pins the registry to exactly the served procedures, a new role-gated RPC is
// enforced the moment its registry entry is added — it cannot ship unguarded —
// and the interceptor tests catch a drifted (resource, action, orgSource).
//
// It runs as a Connect interceptor, i.e. AFTER the authn middleware has populated
// the Principal in context, so the principal is always available here.
func AuthzInterceptor(authorizer *authz.Authorizer, lookup memberRoleLookup) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			spec, ok := permissionRegistry[req.Spec().Procedure]
			if !ok || !spec.IsRoleGated() {
				return next(ctx, req)
			}
			if err := authorizeRoleGated(ctx, authorizer, lookup, req, spec); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	}
}

// authorizeRoleGated enforces one domainRoleGated entry. On the API-key path (no
// customer principal) it is a deliberate no-op: API-key access stays coarse
// project scope, exactly as before Casbin. On the JWT path it resolves the org
// per the entry's orgSource and checks (resource, action) — a non-member is
// denied ORG_NOT_A_MEMBER, an under-privileged member ORG_ROLE_FORBIDDEN (both
// PermissionDenied).
func authorizeRoleGated(
	ctx context.Context,
	authorizer *authz.Authorizer,
	lookup memberRoleLookup,
	req connect.AnyRequest,
	spec authzspec.Spec,
) error {
	principal, err := getPrincipalFromContext(ctx)
	if err != nil {
		return apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
	}
	// API-key (SDK / private-key) path: no customer, no role — coarse project scope.
	if principal.Customer == nil {
		return nil
	}

	orgID, err := resolveOrgID(req, principal, spec.OrgSource())
	if err != nil {
		return err
	}

	_, err = requirePermission(ctx, authorizer, lookup, orgID, spec.Resource(), spec.Action())
	return err
}

// resolveOrgID returns the org a role-gated RPC is authorized in, per its
// orgSource: the x-project-id project's org, or the request message's org_id.
func resolveOrgID(req connect.AnyRequest, principal *Principal, src authzspec.OrgSource) (string, error) {
	switch src {
	case authzspec.OrgFromProject:
		// A customer principal without a project cannot be authorized for a
		// project-scoped RPC; mirror MustGetPrincipalWithProject's Unauthenticated.
		if principal.Project == nil {
			return "", apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
		}
		return principal.Project.OrgID, nil
	case authzspec.OrgFromMessage:
		// Every org-control request message carries org_id (the generated
		// GetOrgId accessor). A registry entry pointing orgFromMessage at a message
		// without it is a wiring bug surfaced as Internal and caught by the tests.
		m, ok := req.Any().(interface{ GetOrgId() string })
		if !ok {
			return "", connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		return m.GetOrgId(), nil
	default:
		return "", connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
