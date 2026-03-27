package orgs

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreorgs "github.com/fivebitsio/cotton/internal/core/orgs"
	orgsv1 "github.com/fivebitsio/cotton/internal/gen/proto/dashboard/orgs/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type server struct {
	service *coreorgs.Service
}

func NewServer(service *coreorgs.Service) *server {
	return &server{service: service}
}

// requireOrgMember extracts the principal and verifies org membership.
func (s *server) requireOrgMember(ctx context.Context, orgID string) (*rpc.Principal, error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	isMember, err := s.service.IsOrgMember(ctx, orgID, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err), slog.String("orgId", orgID), slog.String("customerId", principal.Customer.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	return principal, nil
}

// requireOrgAdmin extracts the principal and verifies admin role via a single GetMemberRole call.
// Returns "not a member" if the customer has no membership, or "admin role required" if member but not admin.
func (s *server) requireOrgAdmin(ctx context.Context, orgID string) (*rpc.Principal, error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	role, err := s.service.GetMemberRole(ctx, orgID, principal.Customer.ID)
	if err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
		}
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err), slog.String("orgId", orgID), slog.String("customerId", principal.Customer.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if role != orgsv1.OrgRole_ORG_ROLE_ADMIN.String() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}

	return principal, nil
}

func (s *server) List(
	ctx context.Context,
	_ *connect.Request[orgsv1.ListRequest],
) (*connect.Response[orgsv1.ListResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	orgs, err := s.service.GetOrgsByCustomerID(ctx, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list orgs", slogx.Error(err), slog.String("customerID", principal.Customer.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.Org, 0, len(orgs))
	for _, o := range orgs {
		result = append(result, toRPCOrg(o))
	}

	return connect.NewResponse(&orgsv1.ListResponse{Orgs: result}), nil
}

func (s *server) Get(
	ctx context.Context,
	req *connect.Request[orgsv1.GetRequest],
) (*connect.Response[orgsv1.GetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := s.requireOrgMember(ctx, req.Msg.OrgId); err != nil {
		return nil, err
	}

	org, err := s.service.GetOrgByID(ctx, req.Msg.OrgId)
	if err != nil {
		if errors.Is(err, coreorgs.ErrOrgNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
		}
		slog.ErrorContext(ctx, "failed to get org", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.GetResponse{Org: toRPCOrg(org)}), nil
}

func (s *server) UpdateDisplayName(
	ctx context.Context,
	req *connect.Request[orgsv1.UpdateDisplayNameRequest],
) (*connect.Response[orgsv1.UpdateDisplayNameResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := s.requireOrgAdmin(ctx, req.Msg.OrgId); err != nil {
		return nil, err
	}

	org, err := s.service.UpdateDisplayName(ctx, req.Msg.OrgId, req.Msg.DisplayName)
	if err != nil {
		if errors.Is(err, coreorgs.ErrOrgNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
		}
		slog.ErrorContext(ctx, "failed to update org", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.UpdateDisplayNameResponse{Org: toRPCOrgFromWrite(org)}), nil
}

func (s *server) ListMembers(
	ctx context.Context,
	req *connect.Request[orgsv1.ListMembersRequest],
) (*connect.Response[orgsv1.ListMembersResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := s.requireOrgMember(ctx, req.Msg.OrgId); err != nil {
		return nil, err
	}

	members, err := s.service.ListMembers(ctx, req.Msg.OrgId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list members", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgMember, 0, len(members))
	for _, m := range members {
		result = append(result, &orgsv1.OrgMember{
			CustomerId:  m.CustomerID,
			DisplayName: m.DisplayName,
			Email:       m.Email,
			OrgId:       m.OrgID,
			Role:        toRPCRole(ctx, m.Role),
		})
	}

	return connect.NewResponse(&orgsv1.ListMembersResponse{Members: result}), nil
}

func (s *server) RemoveMember(
	ctx context.Context,
	req *connect.Request[orgsv1.RemoveMemberRequest],
) (*connect.Response[orgsv1.RemoveMemberResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := s.requireOrgAdmin(ctx, req.Msg.OrgId)
	if err != nil {
		return nil, err
	}

	if req.Msg.CustomerId == principal.Customer.ID {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot remove yourself from an org"))
	}

	if err := s.service.RemoveMemberSafe(ctx, req.Msg.OrgId, req.Msg.CustomerId); err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("member not found"))
		}
		if errors.Is(err, coreorgs.ErrLastAdmin) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot remove the last admin"))
		}
		slog.ErrorContext(ctx, "failed to remove member", slogx.Error(err), slog.String("orgId", req.Msg.OrgId), slog.String("customerId", req.Msg.CustomerId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.RemoveMemberResponse{}), nil
}

func (s *server) InviteMember(
	ctx context.Context,
	req *connect.Request[orgsv1.InviteMemberRequest],
) (*connect.Response[orgsv1.InviteMemberResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := s.requireOrgAdmin(ctx, req.Msg.OrgId)
	if err != nil {
		return nil, err
	}

	inv, err := s.service.InviteMember(ctx, req.Msg.OrgId, principal.Customer.ID, req.Msg.Email)
	if err != nil {
		if errors.Is(err, coreorgs.ErrAlreadyMember) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("this email is already a member of the org"))
		}
		if errors.Is(err, coreorgs.ErrInviteAlreadyPending) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("a pending invitation already exists for this email"))
		}
		slog.ErrorContext(ctx, "failed to create invitation", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.InviteMemberResponse{Invitation: toRPCInvitation(ctx, inv)}), nil
}

func (s *server) AcceptInvite(
	ctx context.Context,
	req *connect.Request[orgsv1.AcceptInviteRequest],
) (*connect.Response[orgsv1.AcceptInviteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	org, err := s.service.AcceptInvite(ctx, req.Msg.Token, principal.Customer.ID, principal.Customer.Email)
	if err != nil {
		if errors.Is(err, coreorgs.ErrInviteWrongEmail) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invitation was issued to a different email address"))
		}
		if errors.Is(err, coreorgs.ErrInviteNotPending) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("invitation is no longer pending"))
		}
		if errors.Is(err, coreorgs.ErrInviteExpired) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("invitation has expired"))
		}
		if errors.Is(err, coreorgs.ErrAlreadyMember) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("already a member of this org"))
		}
		if errors.Is(err, coreorgs.ErrInviteNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("invitation not found"))
		}
		slog.ErrorContext(ctx, "failed to accept invitation", slogx.Error(err), slog.String("customerId", principal.Customer.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.AcceptInviteResponse{Org: toRPCOrg(org)}), nil
}

func (s *server) ListInvitations(
	ctx context.Context,
	req *connect.Request[orgsv1.ListInvitationsRequest],
) (*connect.Response[orgsv1.ListInvitationsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := s.requireOrgAdmin(ctx, req.Msg.OrgId); err != nil {
		return nil, err
	}

	invitations, err := s.service.ListInvitations(ctx, req.Msg.OrgId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invitations", slogx.Error(err), slog.String("orgId", req.Msg.OrgId))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgInvitation, 0, len(invitations))
	for _, inv := range invitations {
		result = append(result, toRPCInvitationRO(ctx, inv))
	}

	return connect.NewResponse(&orgsv1.ListInvitationsResponse{Invitations: result}), nil
}
