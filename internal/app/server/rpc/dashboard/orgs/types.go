package orgs

import (
	orgsv1 "github.com/fivebitsio/cotton/internal/gen/proto/orgs/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

func toRPCOrg(o dbread.Org) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: o.DisplayName,
		Id:          o.ID,
	}
}

func toRPCOrgFromWrite(o dbwrite.Org) *orgsv1.Org {
	return &orgsv1.Org{
		DisplayName: o.DisplayName,
		Id:          o.ID,
	}
}

func toRPCInvitation(inv dbwrite.OrgInvitation) *orgsv1.OrgInvitation {
	var expiresAt string
	if inv.ExpiresAt.Valid {
		expiresAt = inv.ExpiresAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	return &orgsv1.OrgInvitation{
		Email:     inv.Email,
		ExpiresAt: expiresAt,
		Id:        inv.ID,
		OrgId:     inv.OrgID,
		Status:    inv.Status,
		Token:     inv.Token,
	}
}

func toRPCInvitationRO(inv dbread.OrgInvitation) *orgsv1.OrgInvitation {
	var expiresAt string
	if inv.ExpiresAt.Valid {
		expiresAt = inv.ExpiresAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	return &orgsv1.OrgInvitation{
		Email:     inv.Email,
		ExpiresAt: expiresAt,
		Id:        inv.ID,
		OrgId:     inv.OrgID,
		Status:    inv.Status,
	}
}
