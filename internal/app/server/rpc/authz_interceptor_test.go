package rpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/pug-sh/pug/internal/app/server/rpc/authzspec"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func mustAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	a, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	return a
}

// apperrCode extracts a connect.Code from either an *apperr.Error (the authz
// path) or a plain *connect.Error — connect.CodeOf alone returns Unknown for the
// former.
func apperrCode(err error) connect.Code {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code()
	}
	return connect.CodeOf(err)
}

// jwtProjectCtx is the JWT/customer path WITH an x-project-id project resolved —
// the principal a dashboard/shared RPC sees on the customer path.
func jwtProjectCtx() context.Context {
	return authn.SetInfo(context.Background(), &Principal{
		AuthType: AuthTypeJWT,
		Customer: &dbread.Customer{ID: "cust-1"},
		Project:  &dbread.Project{ID: "proj-1", OrgID: "org-1"},
	})
}

// apiKeyCtx is the private-key path: a project but NO customer. The interceptor
// must skip the role gate here (coarse project scope).
func apiKeyCtx() context.Context {
	return authn.SetInfo(context.Background(), &Principal{
		AuthType: AuthTypePrivateKey,
		Project:  &dbread.Project{ID: "proj-1", OrgID: "org-1"},
	})
}

// msgReq builds an org-control request whose message carries org_id, for the
// orgFromMessage path (any such message works — the interceptor only reads
// GetOrgId()).
func msgReq(orgID string) connect.AnyRequest {
	return connect.NewRequest(&orgsv1.ListMembersRequest{OrgId: proto.String(orgID)})
}

// emptyReq builds a request whose message carries no org_id — fine for
// orgFromProject (org comes from the principal) and used to prove the
// orgFromMessage assertion fails closed.
func emptyReq() connect.AnyRequest { return connect.NewRequest(&emptypb.Empty{}) }

// reqFor returns a request appropriate to an entry's orgSource.
func reqFor(spec authzspec.Spec) connect.AnyRequest {
	if spec.OrgSource() == authzspec.OrgFromMessage {
		return msgReq("org-1")
	}
	return emptyReq()
}

// TestAuthorizeRoleGated covers the interceptor's per-call decision against the
// real policy with a faked role lookup (no infra), across both org sources.
func TestAuthorizeRoleGated(t *testing.T) {
	authorizer := mustAuthorizer(t)

	cases := []struct {
		name       string
		ctx        context.Context
		lookup     fakeRoleLookup
		req        connect.AnyRequest
		spec       authzspec.Spec
		wantCode   connect.Code // 0 → expect no error
		wantReason apperr.Reason
	}{
		{
			name:   "api-key path skips the role gate even for a write",
			ctx:    apiKeyCtx(),
			lookup: fakeRoleLookup{err: errors.New("lookup must not be called on the api-key path")},
			req:    emptyReq(),
			spec:   authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete),
		},
		{
			name:     "no principal at all is unauthenticated",
			ctx:      context.Background(),
			lookup:   fakeRoleLookup{err: errors.New("lookup must not be called without a principal")},
			req:      emptyReq(),
			spec:     authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
			wantCode: connect.CodeUnauthenticated, wantReason: apperr.ReasonUnauthenticated,
		},
		{
			name:     "jwt customer without a project is unauthenticated (authzspec.OrgFromProject)",
			ctx:      customerCtx(),
			lookup:   fakeRoleLookup{role: coreorgs.RoleViewer},
			req:      emptyReq(),
			spec:     authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
			wantCode: connect.CodeUnauthenticated, wantReason: apperr.ReasonUnauthenticated,
		},
		{
			name:   "viewer may read dashboards (authzspec.OrgFromProject)",
			ctx:    jwtProjectCtx(),
			lookup: fakeRoleLookup{role: coreorgs.RoleViewer},
			req:    emptyReq(),
			spec:   authzspec.ProjGated(authz.ResourceDashboard, authz.ActionRead),
		},
		{
			name:     "viewer is denied dashboard create",
			ctx:      jwtProjectCtx(),
			lookup:   fakeRoleLookup{role: coreorgs.RoleViewer},
			req:      emptyReq(),
			spec:     authzspec.ProjGated(authz.ResourceDashboard, authz.ActionCreate),
			wantCode: connect.CodePermissionDenied, wantReason: apperr.ReasonOrgRoleForbidden,
		},
		{
			name:   "member may erase profiles",
			ctx:    jwtProjectCtx(),
			lookup: fakeRoleLookup{role: coreorgs.RoleMember},
			req:    emptyReq(),
			spec:   authzspec.ProjGated(authz.ResourceProfile, authz.ActionDelete),
		},
		{
			name:     "non-member is denied (authzspec.OrgFromProject)",
			ctx:      jwtProjectCtx(),
			lookup:   fakeRoleLookup{err: coreorgs.ErrMemberNotFound},
			req:      emptyReq(),
			spec:     authzspec.ProjGated(authz.ResourceInsight, authz.ActionRead),
			wantCode: connect.CodePermissionDenied, wantReason: apperr.ReasonOrgNotAMember,
		},
		{
			name:     "orgFromMessage: viewer denied an admin-only invite",
			ctx:      customerCtx(), // no project — org comes from the message
			lookup:   fakeRoleLookup{role: coreorgs.RoleViewer},
			req:      msgReq("org-1"),
			spec:     authzspec.OrgGated(authz.ResourceInvitation, authz.ActionCreate),
			wantCode: connect.CodePermissionDenied, wantReason: apperr.ReasonOrgRoleForbidden,
		},
		{
			name:     "orgFromMessage: member denied an admin-only invite",
			ctx:      customerCtx(),
			lookup:   fakeRoleLookup{role: coreorgs.RoleMember},
			req:      msgReq("org-1"),
			spec:     authzspec.OrgGated(authz.ResourceInvitation, authz.ActionCreate),
			wantCode: connect.CodePermissionDenied, wantReason: apperr.ReasonOrgRoleForbidden,
		},
		{
			name:   "orgFromMessage: admin allowed an invite",
			ctx:    customerCtx(),
			lookup: fakeRoleLookup{role: coreorgs.RoleAdmin},
			req:    msgReq("org-1"),
			spec:   authzspec.OrgGated(authz.ResourceInvitation, authz.ActionCreate),
		},
		{
			name:   "orgFromMessage: viewer may read the member list",
			ctx:    customerCtx(),
			lookup: fakeRoleLookup{role: coreorgs.RoleViewer},
			req:    msgReq("org-1"),
			spec:   authzspec.OrgGated(authz.ResourceMember, authz.ActionRead),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizeRoleGated(tc.ctx, authorizer, tc.lookup, tc.req, tc.spec)
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("want *apperr.Error, got %T (%v)", err, err)
			}
			if ae.Code() != tc.wantCode {
				t.Errorf("code = %v, want %v", ae.Code(), tc.wantCode)
			}
			if tc.wantReason != "" && ae.Reason() != tc.wantReason {
				t.Errorf("reason = %q, want %q", ae.Reason(), tc.wantReason)
			}
		})
	}
}

// TestResolveOrgID covers org resolution per orgSource, including the fail-closed
// paths a wiring bug would hit.
func TestResolveOrgID(t *testing.T) {
	withProject := &Principal{Project: &dbread.Project{OrgID: "org-1"}}
	noProject := &Principal{Customer: &dbread.Customer{ID: "cust-1"}}

	t.Run("orgFromProject returns the project's org", func(t *testing.T) {
		got, err := resolveOrgID(emptyReq(), withProject, authzspec.OrgFromProject)
		if err != nil || got != "org-1" {
			t.Fatalf("got (%q, %v), want (org-1, nil)", got, err)
		}
	})
	t.Run("orgFromProject without a project is unauthenticated", func(t *testing.T) {
		_, err := resolveOrgID(emptyReq(), noProject, authzspec.OrgFromProject)
		if apperrCode(err) != connect.CodeUnauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", apperrCode(err))
		}
	})
	t.Run("orgFromMessage reads org_id off the message", func(t *testing.T) {
		got, err := resolveOrgID(msgReq("org-9"), noProject, authzspec.OrgFromMessage)
		if err != nil || got != "org-9" {
			t.Fatalf("got (%q, %v), want (org-9, nil)", got, err)
		}
	})
	t.Run("orgFromMessage on a message without org_id fails closed (internal)", func(t *testing.T) {
		_, err := resolveOrgID(emptyReq(), noProject, authzspec.OrgFromMessage)
		if apperrCode(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal", apperrCode(err))
		}
	})
}

// TestAuthzInterceptorRegistryEntriesEnforced ties the registry to the runtime:
// for EVERY domainRoleGated entry it asserts the recorded (resource, action),
// when enforced, (a) lets an admin through, (b) denies a non-member, (c) denies a
// viewer on writes (viewer is the read-only floor), and (d) never allows a viewer
// where it denies a member (no privilege inversion). This is the guarantee that
// makes the registry the source of truth — every role-gated RPC is gated and a
// drifted (resource, action) is caught here, not in production.
func TestAuthzInterceptorRegistryEntriesEnforced(t *testing.T) {
	authorizer := mustAuthorizer(t)

	got := 0
	for proc, spec := range permissionRegistry {
		if !spec.IsRoleGated() {
			continue
		}
		got++
		req := reqFor(spec)

		// (a) admin holds every permission.
		if err := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{role: coreorgs.RoleAdmin}, req, spec); err != nil {
			t.Errorf("%s: admin denied %s:%s (%v)", proc, spec.Resource(), spec.Action(), err)
		}

		// (b) a non-member is always denied ORG_NOT_A_MEMBER.
		nmErr := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{err: coreorgs.ErrMemberNotFound}, req, spec)
		var ae *apperr.Error
		if !errors.As(nmErr, &ae) || ae.Code() != connect.CodePermissionDenied || ae.Reason() != apperr.ReasonOrgNotAMember {
			t.Errorf("%s: non-member not denied ORG_NOT_A_MEMBER (got %v)", proc, nmErr)
		}

		viewerOK := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{role: coreorgs.RoleViewer}, req, spec) == nil
		memberOK := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{role: coreorgs.RoleMember}, req, spec) == nil

		// (c) viewer is read-only: it must hold NO write anywhere.
		if spec.Action() != authz.ActionRead && viewerOK {
			t.Errorf("%s: viewer allowed write %s:%s — viewer must be read-only", proc, spec.Resource(), spec.Action())
		}
		// (d) no privilege inversion: a higher role is never more restricted.
		if viewerOK && !memberOK {
			t.Errorf("%s: viewer allowed but member denied %s:%s — privilege inversion", proc, spec.Resource(), spec.Action())
		}
	}

	if got == 0 {
		t.Fatal("no domainRoleGated entries found — the registry is empty?")
	}
}

// TestRoleGatedAdminOnlyRPCs pins the admin-only control-plane RPCs: a member is
// denied and an admin allowed. This catches a (resource, action) drift that would
// silently let a member perform org/project/email administration — which the
// generic monotonicity check in the test above cannot, since it does not know an
// RPC's intended privilege level.
func TestRoleGatedAdminOnlyRPCs(t *testing.T) {
	authorizer := mustAuthorizer(t)

	adminOnly := []string{
		"/dashboard.orgs.v1.OrgsService/UpdateDisplayName",
		"/dashboard.orgs.v1.OrgsService/InviteMember",
		"/dashboard.orgs.v1.OrgsService/ResendInvite",
		"/dashboard.orgs.v1.OrgsService/ListInvitations",
		"/dashboard.orgs.v1.OrgsService/RemoveMember",
		"/dashboard.orgs.v1.OrgsService/UpdateMemberRole",
		"/dashboard.projects.v1.ProjectsService/Create",
		"/dashboard.projects.v1.ProjectsService/Delete",
		"/dashboard.projects.v1.ProjectsService/UpdateMeta",
		"/dashboard.projects.v1.ProjectsService/UpdateFCMServiceJSON",
		"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Get",
		"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Set",
		"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/Remove",
		"/dashboard.orgemailproviders.v1.OrgEmailProvidersService/SendTest",
	}

	for _, proc := range adminOnly {
		spec, ok := permissionRegistry[proc]
		if !ok || !spec.IsRoleGated() {
			t.Errorf("%s: expected a role-gated registry entry", proc)
			continue
		}
		req := reqFor(spec)
		if err := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{role: coreorgs.RoleMember}, req, spec); apperrCode(err) != connect.CodePermissionDenied {
			t.Errorf("%s: member NOT denied (got %v) — this admin-only RPC must reject a member", proc, err)
		}
		if err := authorizeRoleGated(jwtProjectCtx(), authorizer,
			fakeRoleLookup{role: coreorgs.RoleAdmin}, req, spec); err != nil {
			t.Errorf("%s: admin denied (%v) — this admin-only RPC must allow an admin", proc, err)
		}
	}
}

// TestAuthzInterceptorPassesThrough verifies the dispatch: a procedure that is not
// a domainRoleGated entry (here, an empty/unknown procedure) is passed straight to
// next without consulting the role lookup, so public/self/SDK and domainProject
// RPCs are untouched.
func TestAuthzInterceptorPassesThrough(t *testing.T) {
	authorizer := mustAuthorizer(t)

	called := false
	next := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	// A client-built request has an empty Spec().Procedure, which is not a
	// domainRoleGated entry — the lookup must be skipped entirely.
	req := connect.NewRequest(&emptypb.Empty{})
	wrapped := AuthzInterceptor(authorizer, fakeRoleLookup{err: errors.New("lookup must not be called")})(next)

	if _, err := wrapped(jwtProjectCtx(), req); err != nil {
		t.Fatalf("pass-through returned error: %v", err)
	}
	if !called {
		t.Fatal("next was not called for a non-role-gated procedure")
	}
}
