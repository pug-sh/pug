package orgs

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreorgs "github.com/fivebitsio/cotton/internal/core/orgs"
	orgsv1 "github.com/fivebitsio/cotton/internal/gen/proto/orgs/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
)

type server struct {
	service *coreorgs.Service
}

func NewServer(service *coreorgs.Service) *server {
	return &server{service: service}
}

func (s *server) Get(
	ctx context.Context,
	req *connect.Request[orgsv1.GetRequest],
) (*connect.Response[orgsv1.GetResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isMember, err := s.service.IsOrgMember(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	org, err := s.service.GetOrgByID(ctx, req.Msg.OrgId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
		}
		slog.ErrorContext(ctx, "failed to get org", slogx.Error(err))
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isAdmin, err := s.service.IsOrgAdmin(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}

	org, err := s.service.UpdateDisplayName(ctx, req.Msg.OrgId, req.Msg.DisplayName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
		}
		slog.ErrorContext(ctx, "failed to update org", slogx.Error(err))
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isMember, err := s.service.IsOrgMember(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org membership", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isMember {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
	}

	members, err := s.service.ListMembers(ctx, req.Msg.OrgId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list members", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgMember, 0, len(members))
	for _, m := range members {
		result = append(result, &orgsv1.OrgMember{
			CustomerId:  m.CustomerID,
			DisplayName: m.DisplayName,
			Email:       m.Email,
			OrgId:       m.OrgID,
			Role:        m.Role,
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isAdmin, err := s.service.IsOrgAdmin(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}

	if req.Msg.CustomerId == principal.Customer.ID {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot remove yourself from an org"))
	}

	if err := s.service.RemoveMember(ctx, req.Msg.OrgId, req.Msg.CustomerId); err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("member not found"))
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isAdmin, err := s.service.IsOrgAdmin(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}

	inv, err := s.service.InviteMember(ctx, req.Msg.OrgId, principal.Customer.ID, req.Msg.Email)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create invitation", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.InviteMemberResponse{Invitation: toRPCInvitation(inv)}), nil
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
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	org, err := s.service.AcceptInvite(ctx, req.Msg.Token, principal.Customer.ID)
	if err != nil {
		if errors.Is(err, coreorgs.ErrInviteNotPending) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("invitation is no longer pending"))
		}
		if errors.Is(err, coreorgs.ErrInviteExpired) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("invitation has expired"))
		}
		if errors.Is(err, coreorgs.ErrAlreadyMember) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("already a member of this org"))
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("invitation not found"))
		}
		slog.ErrorContext(ctx, "failed to accept invitation", slogx.Error(err))
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	isAdmin, err := s.service.IsOrgAdmin(ctx, req.Msg.OrgId, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !isAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}

	invitations, err := s.service.ListInvitations(ctx, req.Msg.OrgId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invitations", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgInvitation, 0, len(invitations))
	for _, inv := range invitations {
		result = append(result, toRPCInvitationRO(inv))
	}

	return connect.NewResponse(&orgsv1.ListInvitationsResponse{Invitations: result}), nil
}
