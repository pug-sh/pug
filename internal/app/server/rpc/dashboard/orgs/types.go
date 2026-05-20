package orgs

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/proto"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// roleFromProto translates a proto OrgRole enum into the service-layer Role.
// Returns ok=false for UNSPECIFIED or any enum value not in the recognized
// set; callers MUST reject the request explicitly rather than passing the
// zero-value Role onward. Protovalidate's not_in:[0] / defined_only catches
// most of this at the interceptor, but the second check here keeps the
// type-level invariant load-bearing even if validation is bypassed.
func roleFromProto(p orgsv1.OrgRole) (coreorgs.Role, bool) {
	switch p {
	case orgsv1.OrgRole_ORG_ROLE_ADMIN:
		return coreorgs.RoleAdmin, true
	case orgsv1.OrgRole_ORG_ROLE_MEMBER:
		return coreorgs.RoleMember, true
	default:
		return "", false
	}
}

// roleFromDBJoinRow parses a raw role string from a DB-join row (where the
// role column is selected as plain text rather than going through
// Service.GetMemberRole's typed parse). On unknown values, logs a WarnContext
// and returns the empty Role so toRPCRole maps it to UNSPECIFIED on the wire.
// Read-path drift is soft-failed (rather than hard-failed) because List/Get
// are user-facing reads where surfacing a server-side error would block the
// dashboard; the WarnContext + UNSPECIFIED combo is the operator signal.
func roleFromDBJoinRow(ctx context.Context, raw string) coreorgs.Role {
	role, err := coreorgs.ParseRole(raw)
	if err != nil {
		slog.WarnContext(ctx, "unknown org role from database, falling back to UNSPECIFIED", slog.String("role", raw))
		return ""
	}
	return role
}

// toRPCOrg converts a dbread.Org plus the caller's role to the proto Org.
func toRPCOrg(o dbread.Org, role coreorgs.Role) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
		Role:        toRPCRole(role).Enum(),
	}
}

func toRPCOrgFromWrite(o dbwrite.Org, role coreorgs.Role) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
		Role:        toRPCRole(role).Enum(),
	}
}

// toRPCOrgWithRole converts the joined read-row (which already carries the
// caller's role) into the proto Org. The role is parsed at this boundary so
// drift surfaces as UNSPECIFIED on the wire with an operator-visible log.
func toRPCOrgWithRole(ctx context.Context, row dbread.GetOrgWithRoleByIDAndCustomerIDRow) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(row.DisplayName),
		Id:          proto.String(row.ID),
		Role:        toRPCRole(roleFromDBJoinRow(ctx, row.Role)).Enum(),
	}
}

// toRPCRole maps a validated coreorgs.Role to its proto enum equivalent. The
// empty Role (returned by roleFromDBJoinRow on drift) maps to UNSPECIFIED.
// Inputs are already validated by ParseRole / roleFromProto upstream so no
// further logging or error path is needed here.
func toRPCRole(role coreorgs.Role) orgsv1.OrgRole {
	switch role {
	case coreorgs.RoleAdmin:
		return orgsv1.OrgRole_ORG_ROLE_ADMIN
	case coreorgs.RoleMember:
		return orgsv1.OrgRole_ORG_ROLE_MEMBER
	default:
		return orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED
	}
}

func toRPCInvitationStatus(ctx context.Context, status string) orgsv1.InvitationStatus {
	if v, ok := orgsv1.InvitationStatus_value[status]; ok {
		return orgsv1.InvitationStatus(v)
	}
	slog.WarnContext(ctx, "unknown invitation status from database, falling back to UNSPECIFIED", slog.String("status", status))
	return orgsv1.InvitationStatus_INVITATION_STATUS_UNSPECIFIED
}

// toRPCInvitation includes the Token field — use only for InviteMember responses
// where the admin needs the token to share the invite link. For list responses,
// use toRPCInvitationRO which omits the token to prevent leakage.
func toRPCInvitation(ctx context.Context, inv dbwrite.OrgInvitation) *orgsv1.OrgInvitation {
	var expiresAt string
	if inv.ExpiresAt.Valid {
		expiresAt = inv.ExpiresAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	return &orgsv1.OrgInvitation{
		Email:     proto.String(inv.Email),
		ExpiresAt: proto.String(expiresAt),
		Id:        proto.String(inv.ID),
		OrgId:     proto.String(inv.OrgID),
		Status:    toRPCInvitationStatus(ctx, inv.Status).Enum(),
		Token:     proto.String(inv.Token),
	}
}

func toRPCInvitationRO(ctx context.Context, inv dbread.OrgInvitation) *orgsv1.OrgInvitation {
	var expiresAt string
	if inv.ExpiresAt.Valid {
		expiresAt = inv.ExpiresAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	return &orgsv1.OrgInvitation{
		Email:     proto.String(inv.Email),
		ExpiresAt: proto.String(expiresAt),
		Id:        proto.String(inv.ID),
		OrgId:     proto.String(inv.OrgID),
		Status:    toRPCInvitationStatus(ctx, inv.Status).Enum(),
	}
}
