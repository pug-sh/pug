package orgs

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/proto"

	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// toRPCOrg and toRPCOrgFromWrite must be kept in sync — they convert
// the read and write models to the same proto message.
func toRPCOrg(o dbread.Org) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
	}
}

func toRPCOrgFromWrite(o dbwrite.Org) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: proto.String(o.DisplayName),
		Id:          proto.String(o.ID),
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

// toRPCInvitation and toRPCInvitationRO must be kept in sync — they convert
// the write and read invitation models to the same proto message. The proto
// OrgInvitation has no token field; invite acceptance is driven by the emailed
// link, never by a value returned from these endpoints.
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
