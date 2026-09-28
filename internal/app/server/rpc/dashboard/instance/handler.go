package instance

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/deletion"
	"github.com/pug-sh/pug/internal/core/instanceadmin"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	instancev1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/instance/v1"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"google.golang.org/protobuf/proto"
)

type server struct {
	service   *instanceadmin.Service
	deletions *deletion.Service
}

func NewServer(service *instanceadmin.Service, deletions *deletion.Service) *server {
	return &server{service: service, deletions: deletions}
}

func actor(ctx context.Context) (string, error) {
	p, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return "", err
	}
	return p.Customer.ID, nil
}

func internal(err error) error {
	if errors.Is(err, instanceadmin.ErrNotFound) {
		return apperr.NotFound(apperr.ReasonInstanceResourceNotFound, "resource not found")
	}
	if errors.Is(err, instanceadmin.ErrOrgInactive) {
		return apperr.FailedPrecondition(apperr.ReasonDeletionBlocked, "organization is pending deletion")
	}
	if errors.Is(err, coreorgs.ErrOrgNotFound) {
		return apperr.NotFound(apperr.ReasonOrgNotFound, "organization not found")
	}
	if errors.Is(err, instanceadmin.ErrInvalidPageToken) {
		return apperr.Invalid(apperr.ReasonInstanceInvalidPageToken, "invalid page token")
	}
	if errors.Is(err, instanceadmin.ErrLastAdmin) {
		return apperr.FailedPrecondition(apperr.ReasonLastInstanceAdmin, "cannot disable the last instance administrator")
	}
	if errors.Is(err, coreorgs.ErrLastAdmin) {
		return apperr.FailedPrecondition(apperr.ReasonCannotRemoveLastAdmin, "cannot remove the last organization admin")
	}
	if errors.Is(err, coreorgs.ErrMemberNotFound) {
		return apperr.NotFound(apperr.ReasonOrgMemberNotFound, "member not found")
	}
	if errors.Is(err, coreorgs.ErrInviteNotFound) {
		return apperr.NotFound(apperr.ReasonInvitationNotFound, "invitation not found")
	}
	if errors.Is(err, coreorgs.ErrInviteNotPending) {
		return apperr.FailedPrecondition(apperr.ReasonInvitationNotPending, "invitation is no longer pending")
	}
	if errors.Is(err, coreorgs.ErrInviteSendLimit) {
		return apperr.FailedPrecondition(apperr.ReasonInvitationSendLimit, "invitation has reached its send limit")
	}
	if errors.Is(err, coreorgs.ErrInviteAlreadyPending) {
		return apperr.AlreadyExists(apperr.ReasonInvitationAlreadyPending, "invitation already pending")
	}
	if errors.Is(err, coreorgs.ErrAlreadyMember) {
		return apperr.AlreadyExists(apperr.ReasonOrgMemberAlreadyExists, "already a member")
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func role(value string) orgsv1.OrgRole {
	if n, ok := orgsv1.OrgRole_value[value]; ok {
		return orgsv1.OrgRole(n)
	}
	return orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED
}

func toUser(user instanceadmin.User) *instancev1.User {
	memberships := make([]*instancev1.Membership, 0, len(user.Memberships))
	for _, m := range user.Memberships {
		memberships = append(memberships, &instancev1.Membership{OrgId: proto.String(m.OrgID), OrgName: proto.String(m.OrgName), Role: role(m.Role).Enum()})
	}
	return &instancev1.User{Id: proto.String(user.ID), Email: proto.String(user.Email), CreatedAt: proto.String(user.CreatedAt.Format(time.RFC3339)), EmailVerified: proto.Bool(user.Verified), Disabled: proto.Bool(user.Disabled), Memberships: memberships}
}

func toOrganization(o instanceadmin.Organization) *instancev1.Organization {
	return &instancev1.Organization{Id: proto.String(o.ID), Name: proto.String(o.Name), CreatedAt: proto.String(o.CreatedAt.Format(time.RFC3339)), MemberCount: proto.Uint32(uint32(o.MemberCount)), ProjectCount: proto.Uint32(uint32(o.ProjectCount)), AdminEmails: o.AdminEmails, NeedsAdmin: proto.Bool(o.NeedsAdmin), DeletionState: proto.String(o.DeletionState)}
}

func toInvitation(id, email string, expires time.Time, invitationRole string) *instancev1.Invitation {
	return &instancev1.Invitation{Id: proto.String(id), Email: proto.String(email), ExpiresAt: proto.String(expires.Format(time.RFC3339)), Role: role(invitationRole).Enum()}
}

func boolFilter(value instancev1.BooleanFilter) (*bool, error) {
	switch value {
	case instancev1.BooleanFilter_BOOLEAN_FILTER_UNSPECIFIED:
		return nil, nil
	case instancev1.BooleanFilter_BOOLEAN_FILTER_TRUE:
		v := true
		return &v, nil
	case instancev1.BooleanFilter_BOOLEAN_FILTER_FALSE:
		v := false
		return &v, nil
	default:
		return nil, apperr.Invalid(apperr.ReasonInstanceInvalidUserFilter, "invalid user filter")
	}
}

func (s *server) ListUsers(ctx context.Context, req *connect.Request[instancev1.ListUsersRequest]) (*connect.Response[instancev1.ListUsersResponse], error) {
	verified, err := boolFilter(req.Msg.GetVerified())
	if err != nil {
		return nil, err
	}
	enabled, err := boolFilter(req.Msg.GetEnabled())
	if err != nil {
		return nil, err
	}
	rows, next, err := s.service.ListUsers(ctx, instanceadmin.UserFilters{
		Search: req.Msg.GetSearch(), OrgID: req.Msg.GetOrgId(), Verified: verified, Enabled: enabled,
	}, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, internal(err)
	}
	users := make([]*instancev1.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, toUser(row))
	}
	return connect.NewResponse(&instancev1.ListUsersResponse{Users: users, NextPageToken: proto.String(next)}), nil
}

func (s *server) SetUserDisabled(ctx context.Context, req *connect.Request[instancev1.SetUserDisabledRequest]) (*connect.Response[instancev1.SetUserDisabledResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.service.SetUserDisabled(ctx, id, req.Msg.GetUserId(), req.Msg.GetDisabled()); err != nil {
		return nil, internal(err)
	}
	user, err := s.service.GetUser(ctx, req.Msg.GetUserId())
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.SetUserDisabledResponse{User: toUser(user)}), nil
}

func (s *server) RevokeUserSessions(ctx context.Context, req *connect.Request[instancev1.RevokeUserSessionsRequest]) (*connect.Response[instancev1.RevokeUserSessionsResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.service.RevokeUserSessions(ctx, id, req.Msg.GetUserId()); err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.RevokeUserSessionsResponse{}), nil
}

func (s *server) ListOrganizations(ctx context.Context, req *connect.Request[instancev1.ListOrganizationsRequest]) (*connect.Response[instancev1.ListOrganizationsResponse], error) {
	rows, next, err := s.service.ListOrganizations(ctx, req.Msg.GetSearch(), req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, internal(err)
	}
	orgs := make([]*instancev1.Organization, 0, len(rows))
	for _, row := range rows {
		orgs = append(orgs, toOrganization(row))
	}
	return connect.NewResponse(&instancev1.ListOrganizationsResponse{Organizations: orgs, NextPageToken: proto.String(next)}), nil
}

func (s *server) GetOrganization(ctx context.Context, req *connect.Request[instancev1.GetOrganizationRequest]) (*connect.Response[instancev1.GetOrganizationResponse], error) {
	detail, err := s.service.GetOrganization(ctx, req.Msg.GetOrgId(), req.Msg.GetPageSize(), req.Msg.GetProjectPageToken(), req.Msg.GetMemberPageToken(), req.Msg.GetInvitationPageToken())
	if err != nil {
		return nil, internal(err)
	}
	result := &instancev1.GetOrganizationResponse{Organization: toOrganization(detail.Organization), NextProjectPageToken: proto.String(detail.NextProjectPageToken), NextMemberPageToken: proto.String(detail.NextMemberPageToken), NextInvitationPageToken: proto.String(detail.NextInvitationPageToken)}
	for _, p := range detail.Projects {
		result.Projects = append(result.Projects, &instancev1.Project{Id: proto.String(p.ID), Name: proto.String(p.Name), CreatedAt: proto.String(p.CreatedAt.Format(time.RFC3339)), ReportingTimezone: proto.String(p.ReportingTimezone), DeletionState: proto.String(p.DeletionState)})
	}
	for _, u := range detail.Members {
		result.Members = append(result.Members, toUser(u))
	}
	for _, inv := range detail.Invitations {
		result.Invitations = append(result.Invitations, toInvitation(inv.ID, inv.Email, inv.ExpiresAt, inv.Role))
	}
	return connect.NewResponse(result), nil
}

func (s *server) ProvisionOrganization(ctx context.Context, req *connect.Request[instancev1.ProvisionOrganizationRequest]) (*connect.Response[instancev1.ProvisionOrganizationResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := s.service.ProvisionOrganization(ctx, id, req.Msg.GetName(), req.Msg.GetAdminEmail())
	if err != nil && !errors.Is(err, instanceadmin.ErrInitialInvitationFailed) {
		return nil, internal(err)
	}
	invitationCreated := err == nil
	detail, err := s.service.GetOrganization(ctx, orgID, 0)
	if err != nil {
		return nil, internal(err)
	}
	response := &instancev1.ProvisionOrganizationResponse{Organization: toOrganization(detail.Organization), InitialInvitationCreated: proto.Bool(invitationCreated)}
	if !invitationCreated {
		response.Warning = proto.String("Organization created, but the initial invitation failed. Invite an admin from the organization detail.")
	}
	return connect.NewResponse(response), nil
}

func (s *server) RenameOrganization(ctx context.Context, req *connect.Request[instancev1.RenameOrganizationRequest]) (*connect.Response[instancev1.RenameOrganizationResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.service.RenameOrganization(ctx, id, req.Msg.GetOrgId(), req.Msg.GetName()); err != nil {
		return nil, internal(err)
	}
	detail, err := s.service.GetOrganization(ctx, req.Msg.GetOrgId(), 0)
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.RenameOrganizationResponse{Organization: toOrganization(detail.Organization)}), nil
}

func requestedRole(r orgsv1.OrgRole) (coreorgs.Role, error) {
	if r == orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED {
		return coreorgs.RoleMember, nil
	}
	result := coreorgs.Role(r.String())
	if !result.IsValid() {
		return "", apperr.Invalid(apperr.ReasonOrgUnsupportedRole, "unsupported role")
	}
	return result, nil
}

func requiredRole(r orgsv1.OrgRole) (coreorgs.Role, error) {
	if r == orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED {
		return "", apperr.Invalid(apperr.ReasonOrgUnsupportedRole, "role is required")
	}
	return requestedRole(r)
}

func (s *server) InviteMember(ctx context.Context, req *connect.Request[instancev1.InviteMemberRequest]) (*connect.Response[instancev1.InviteMemberResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	memberRole, err := requestedRole(req.Msg.GetRole())
	if err != nil {
		return nil, err
	}
	dispatch, err := s.service.InviteMember(ctx, id, req.Msg.GetOrgId(), req.Msg.GetEmail(), memberRole)
	if err != nil {
		return nil, internal(err)
	}
	inv := dispatch.Invitation
	return connect.NewResponse(&instancev1.InviteMemberResponse{Invitation: toInvitation(inv.ID, inv.Email, inv.ExpiresAt.Time, inv.Role)}), nil
}

func (s *server) ResendInvitation(ctx context.Context, req *connect.Request[instancev1.ResendInvitationRequest]) (*connect.Response[instancev1.ResendInvitationResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	dispatch, err := s.service.ResendInvitation(ctx, id, req.Msg.GetOrgId(), req.Msg.GetInvitationId())
	if err != nil {
		return nil, internal(err)
	}
	inv := dispatch.Invitation
	return connect.NewResponse(&instancev1.ResendInvitationResponse{Invitation: toInvitation(inv.ID, inv.Email, inv.ExpiresAt.Time, inv.Role)}), nil
}

func (s *server) RevokeInvitation(ctx context.Context, req *connect.Request[instancev1.RevokeInvitationRequest]) (*connect.Response[instancev1.RevokeInvitationResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.service.RevokeInvitation(ctx, id, req.Msg.GetOrgId(), req.Msg.GetInvitationId()); err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.RevokeInvitationResponse{}), nil
}

func (s *server) SetMemberRole(ctx context.Context, req *connect.Request[instancev1.SetMemberRoleRequest]) (*connect.Response[instancev1.SetMemberRoleResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	memberRole, err := requiredRole(req.Msg.GetRole())
	if err != nil {
		return nil, err
	}
	if err := s.service.SetMemberRole(ctx, id, req.Msg.GetOrgId(), req.Msg.GetUserId(), memberRole); err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.SetMemberRoleResponse{}), nil
}

func (s *server) RemoveMember(ctx context.Context, req *connect.Request[instancev1.RemoveMemberRequest]) (*connect.Response[instancev1.RemoveMemberResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.service.RemoveMember(ctx, id, req.Msg.GetOrgId(), req.Msg.GetUserId()); err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&instancev1.RemoveMemberResponse{}), nil
}

func deletionError(err error) error {
	switch {
	case errors.Is(err, deletion.ErrNotFound):
		return apperr.NotFound(apperr.ReasonInstanceResourceNotFound, err.Error())
	case errors.Is(err, deletion.ErrNameMismatch), errors.Is(err, deletion.ErrReasonRequired):
		return apperr.Invalid(apperr.ReasonDeletionConfirmationMismatch, err.Error())
	case errors.Is(err, deletion.ErrAlreadyRequested), errors.Is(err, deletion.ErrComplianceActive), errors.Is(err, deletion.ErrBillingActive), errors.Is(err, deletion.ErrCannotCancel), errors.Is(err, deletion.ErrCannotRetry):
		return apperr.FailedPrecondition(apperr.ReasonDeletionBlocked, err.Error())
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}

func toDeletion(o deletion.Operation) *instancev1.DeletionOperation {
	r := &instancev1.DeletionOperation{Id: proto.String(o.ID), TargetType: proto.String(o.TargetType), TargetId: proto.String(o.TargetID), TargetName: proto.String(o.TargetName), OrgId: proto.String(o.OrgID), Status: proto.String(o.Status), RequestedAt: proto.String(o.Requested.Format(time.RFC3339)), PurgeAfter: proto.String(o.PurgeAfter.Format(time.RFC3339)), LastError: proto.String(o.LastError), ActorId: proto.String(o.ActorID), ActorEmail: proto.String(o.ActorEmail), Reason: proto.String(o.Reason)}
	if o.Finished != nil {
		r.FinishedAt = proto.String(o.Finished.Format(time.RFC3339))
	}
	for _, p := range o.Projects {
		r.Projects = append(r.Projects, &instancev1.DeletionProjectStep{ProjectId: proto.String(p.ProjectID), ProjectName: proto.String(p.ProjectName), ClickhouseDone: proto.Bool(p.ClickHouseDone != nil), PostgresDone: proto.Bool(p.PostgresDone != nil)})
	}
	return r
}

func (s *server) RequestProjectDeletion(ctx context.Context, req *connect.Request[instancev1.RequestProjectDeletionRequest]) (*connect.Response[instancev1.RequestProjectDeletionResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.deletions.RequestProject(ctx, id, req.Msg.GetOrgId(), req.Msg.GetProjectId(), req.Msg.GetConfirmationName())
	if err != nil {
		return nil, deletionError(err)
	}
	return connect.NewResponse(&instancev1.RequestProjectDeletionResponse{Operation: toDeletion(o)}), nil
}

func (s *server) RequestOrganizationDeletion(ctx context.Context, req *connect.Request[instancev1.RequestOrganizationDeletionRequest]) (*connect.Response[instancev1.RequestOrganizationDeletionResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.deletions.RequestOrganization(ctx, id, req.Msg.GetOrgId(), req.Msg.GetConfirmationId(), req.Msg.GetReason())
	if err != nil {
		return nil, deletionError(err)
	}
	return connect.NewResponse(&instancev1.RequestOrganizationDeletionResponse{Operation: toDeletion(o)}), nil
}

func (s *server) CancelOrganizationDeletion(ctx context.Context, req *connect.Request[instancev1.CancelOrganizationDeletionRequest]) (*connect.Response[instancev1.CancelOrganizationDeletionResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.deletions.CancelOrganization(ctx, id, req.Msg.GetOperationId())
	if err != nil {
		return nil, deletionError(err)
	}
	return connect.NewResponse(&instancev1.CancelOrganizationDeletionResponse{Operation: toDeletion(o)}), nil
}

func (s *server) RetryDeletion(ctx context.Context, req *connect.Request[instancev1.RetryDeletionRequest]) (*connect.Response[instancev1.RetryDeletionResponse], error) {
	id, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.deletions.Retry(ctx, id, req.Msg.GetOperationId())
	if err != nil {
		return nil, deletionError(err)
	}
	return connect.NewResponse(&instancev1.RetryDeletionResponse{Operation: toDeletion(o)}), nil
}

func (s *server) ListDeletions(ctx context.Context, req *connect.Request[instancev1.ListDeletionsRequest]) (*connect.Response[instancev1.ListDeletionsResponse], error) {
	rows, next, err := s.deletions.ListPage(ctx, req.Msg.GetOrgId(), int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, deletionError(err)
	}
	r := &instancev1.ListDeletionsResponse{NextPageToken: proto.String(next)}
	for _, o := range rows {
		r.Operations = append(r.Operations, toDeletion(o))
	}
	return connect.NewResponse(r), nil
}
