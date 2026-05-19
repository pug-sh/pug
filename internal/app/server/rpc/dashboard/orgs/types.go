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

// toRPCOrg converts a dbread.Org plus the caller's role to the proto Org.
func toRPCOrg(ctx context.Context, o dbread.Org, role string) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
		Role:        toRPCRole(ctx, role).Enum(),
	}
}

func toRPCOrgFromWrite(ctx context.Context, o dbwrite.Org, role string) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
		Role:        toRPCRole(ctx, role).Enum(),
	}
}

// toRPCOrgWithRole converts the joined read-row (which already carries the
// caller's role) into the proto Org.
func toRPCOrgWithRole(ctx context.Context, row dbread.GetOrgWithRoleByIDAndCustomerIDRow) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(row.DisplayName),
		Id:          proto.String(row.ID),
		Role:        toRPCRole(ctx, row.Role).Enum(),
	}
}

func toRPCRole(ctx context.Context, role string) orgsv1.OrgRole {
	if v, ok := orgsv1.OrgRole_value[role]; ok {
		return orgsv1.OrgRole(v)
	}
	slog.WarnContext(ctx, "unknown org role from database, falling back to UNSPECIFIED", slog.String("role", role))
	return orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED
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
