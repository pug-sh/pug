package orgs

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

// Authorization (org-role gating) is enforced centrally by rpc.AuthzInterceptor
// from the permission registry, before any handler runs — these handlers assume
// the caller is already authorized for the (resource, action) recorded there.
type server struct {
	service *coreorgs.Service
}

func NewServer(service *coreorgs.Service) *server {
	return &server{service: service}
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
		return nil, err
	}

	rows, err := s.service.GetOrgsWithRole(ctx, principal.Customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list orgs", slogx.Error(err), slog.String("customer_id", principal.Customer.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.Org, 0, len(rows))
	for _, r := range rows {
		result = append(result, &orgsv1.Org{
			DisplayName: proto.String(r.DisplayName),
			Id:          proto.String(r.ID),
			Role:        toRPCRole(roleFromDBJoinRow(ctx, r.Role)).Enum(),
		})
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

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	row, err := s.service.GetOrgWithRole(ctx, req.Msg.GetOrgId(), principal.Customer.ID)
	if err != nil {
		if errors.Is(err, coreorgs.ErrOrgNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", req.Msg.GetOrgId()))
		}
		// Service logs+records at source per the log-at-source convention.
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.GetResponse{Org: toRPCOrgWithRole(ctx, row)}), nil
}

func (s *server) UpdateDisplayName(
	ctx context.Context,
	req *connect.Request[orgsv1.UpdateDisplayNameRequest],
) (*connect.Response[orgsv1.UpdateDisplayNameResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	org, err := s.service.UpdateDisplayName(ctx, req.Msg.GetOrgId(), req.Msg.GetDisplayName())
	if err != nil {
		if errors.Is(err, coreorgs.ErrOrgNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", req.Msg.GetOrgId()))
		}
		slog.ErrorContext(ctx, "failed to update org", slogx.Error(err), slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.UpdateDisplayNameResponse{Org: toRPCOrgFromWrite(org, coreorgs.RoleAdmin)}), nil
}

func (s *server) ListMembers(
	ctx context.Context,
	req *connect.Request[orgsv1.ListMembersRequest],
) (*connect.Response[orgsv1.ListMembersResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	members, err := s.service.ListMembers(ctx, req.Msg.GetOrgId())
	if err != nil {
		slog.ErrorContext(ctx, "failed to list members", slogx.Error(err), slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgMember, 0, len(members))
	for _, m := range members {
		result = append(result, &orgsv1.OrgMember{
			CustomerId:  proto.String(m.CustomerID),
			DisplayName: proto.String(m.DisplayName),
			Email:       proto.String(m.Email),
			OrgId:       proto.String(m.OrgID),
			Role:        toRPCRole(roleFromDBJoinRow(ctx, m.Role)).Enum(),
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
		return nil, err
	}

	if req.Msg.GetCustomerId() == principal.Customer.ID {
		return nil, apperr.Invalid(apperr.ReasonOrgCannotRemoveSelf, "cannot remove yourself from an org")
	}

	if err := s.service.RemoveMemberSafe(ctx, req.Msg.GetOrgId(), req.Msg.GetCustomerId()); err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgMemberNotFound, "member not found", apperr.Resource("org_member", req.Msg.GetCustomerId()))
		}
		if errors.Is(err, coreorgs.ErrLastAdmin) {
			return nil, apperr.FailedPrecondition(apperr.ReasonCannotRemoveLastAdmin, "cannot remove the last admin")
		}
		// Service logs+records at source per the log-at-source convention.
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
		return nil, err
	}

	role, ok := inviteRoleFromProto(req.Msg.GetRole())
	if !ok {
		return nil, apperr.Invalid(apperr.ReasonOrgUnsupportedRole, "role is not supported")
	}

	dispatch, err := s.service.InviteMemberWithRole(ctx, req.Msg.GetOrgId(), principal.Customer.ID, req.Msg.GetEmail(), role)
	if err != nil {
		if errors.Is(err, coreorgs.ErrAlreadyMember) {
			return nil, apperr.AlreadyExists(apperr.ReasonOrgMemberAlreadyExists, "this email is already a member of the org")
		}
		if errors.Is(err, coreorgs.ErrInviteAlreadyPending) {
			return nil, apperr.AlreadyExists(apperr.ReasonInvitationAlreadyPending, "a pending invitation already exists for this email")
		}
		// Service logs+records at source per the log-at-source convention.
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.InviteMemberResponse{Invitation: toRPCInvitation(ctx, dispatch.Invitation)}), nil
}

func (s *server) ResendInvite(
	ctx context.Context,
	req *connect.Request[orgsv1.ResendInviteRequest],
) (*connect.Response[orgsv1.ResendInviteResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dispatch, err := s.service.ResendInvite(ctx, req.Msg.GetOrgId(), req.Msg.GetInvitationId())
	if err != nil {
		if errors.Is(err, coreorgs.ErrInviteNotFound) {
			return nil, apperr.NotFound(apperr.ReasonInvitationNotFound, "invitation not found", apperr.Resource("invitation", req.Msg.GetInvitationId()))
		}
		if errors.Is(err, coreorgs.ErrInviteNotPending) {
			return nil, apperr.FailedPrecondition(apperr.ReasonInvitationNotPending, "invitation is no longer pending")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.ResendInviteResponse{Invitation: toRPCInvitation(ctx, dispatch.Invitation)}), nil
}

func (s *server) ListInvitations(
	ctx context.Context,
	req *connect.Request[orgsv1.ListInvitationsRequest],
) (*connect.Response[orgsv1.ListInvitationsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	invitations, err := s.service.ListInvitations(ctx, req.Msg.GetOrgId())
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invitations", slogx.Error(err), slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	result := make([]*orgsv1.OrgInvitation, 0, len(invitations))
	for _, inv := range invitations {
		result = append(result, toRPCInvitationRO(ctx, inv))
	}

	return connect.NewResponse(&orgsv1.ListInvitationsResponse{Invitations: result}), nil
}

func (s *server) Create(
	ctx context.Context,
	req *connect.Request[orgsv1.CreateRequest],
) (*connect.Response[orgsv1.CreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	// display_name validation (required, min_len=1, max_len=150) is enforced
	// by the protovalidate interceptor on CreateRequest before this handler runs.
	org, err := s.service.CreateOrgWithDefaults(ctx, principal.Customer.ID, req.Msg.GetDisplayName())
	if err != nil {
		// Defense-in-depth: the default-project insert into a brand-new org
		// cannot collide on (org_id, display_name) under current code, but if
		// a future change ever surfaces ErrProjectNameTaken to this handler
		// translate it to AlreadyExists so the user sees actionable feedback.
		if errors.Is(err, coreprojects.ErrProjectNameTaken) {
			return nil, apperr.AlreadyExists(apperr.ReasonProjectNameTaken, "default project name already exists in this org")
		}
		// Service logs+records at source per the log-at-source convention.
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.CreateResponse{
		Org: toRPCOrgFromWrite(org, coreorgs.RoleAdmin),
	}), nil
}

func (s *server) Leave(
	ctx context.Context,
	req *connect.Request[orgsv1.LeaveRequest],
) (*connect.Response[orgsv1.LeaveResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.Leave(ctx, req.Msg.GetOrgId(), principal.Customer.ID); err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgMemberNotFound, "not a member of this org")
		}
		if errors.Is(err, coreorgs.ErrLastAdmin) {
			return nil, apperr.FailedPrecondition(apperr.ReasonCannotRemoveLastAdmin, "cannot leave as the last admin")
		}
		if errors.Is(err, coreorgs.ErrLastMember) {
			return nil, apperr.FailedPrecondition(apperr.ReasonCannotLeaveAsLastMember, "cannot leave as the only member")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.LeaveResponse{}), nil
}

func (s *server) UpdateMemberRole(
	ctx context.Context,
	req *connect.Request[orgsv1.UpdateMemberRoleRequest],
) (*connect.Response[orgsv1.UpdateMemberRoleResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	newRole, ok := roleFromProto(req.Msg.GetRole())
	if !ok {
		return nil, apperr.Invalid(apperr.ReasonOrgUnsupportedRole, "role enum value not supported by this server")
	}

	if _, err := s.service.UpdateMemberRole(
		ctx,
		req.Msg.GetOrgId(),
		req.Msg.GetCustomerId(),
		newRole,
	); err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgMemberNotFound, "member not found", apperr.Resource("org_member", req.Msg.GetCustomerId()))
		}
		if errors.Is(err, coreorgs.ErrLastAdmin) {
			return nil, apperr.FailedPrecondition(apperr.ReasonCannotDemoteLastAdmin, "cannot demote the last admin")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	// Re-fetch with the customer join so the response matches the shape of
	// ListMembersResponse.members[] (display_name + email populated).
	row, err := s.service.GetMember(ctx, req.Msg.GetOrgId(), req.Msg.GetCustomerId())
	if err != nil {
		// ErrMemberNotFound here would mean a concurrent removal between the
		// update and the re-fetch — translate accordingly; everything else is
		// already logged at the service layer.
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgMemberNotFound, "member not found", apperr.Resource("org_member", req.Msg.GetCustomerId()))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&orgsv1.UpdateMemberRoleResponse{
		Member: &orgsv1.OrgMember{
			CustomerId:  proto.String(row.CustomerID),
			DisplayName: proto.String(row.DisplayName),
			Email:       proto.String(row.Email),
			OrgId:       proto.String(row.OrgID),
			Role:        toRPCRole(roleFromDBJoinRow(ctx, row.Role)).Enum(),
		},
	}), nil
}
