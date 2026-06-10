package auth

import (
	"context"
	"errors"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// InviteContext carries org-invite metadata for signup provisioning.
type InviteContext struct {
	OrgInvitationID string
}

// FinishSignup applies org join (invite) or default org for newly-created
// accounts. reportingTimezone seeds the default project's reporting timezone on
// the plain-signup path (coerced to UTC if malformed); it is ignored on the
// invite path, which joins an existing org rather than creating one.
func FinishSignup(ctx context.Context, w *dbwrite.Queries, customerID string, createdNew bool, invite *InviteContext, reportingTimezone string) error {
	if invite != nil && invite.OrgInvitationID != "" {
		if err := coreorgs.ApplyInviteAcceptanceInTx(ctx, w, invite.OrgInvitationID, customerID); err != nil {
			switch {
			case errors.Is(err, coreorgs.ErrAlreadyMember):
				return nil
			case errors.Is(err, coreorgs.ErrInviteNotFound),
				errors.Is(err, coreorgs.ErrInviteNotPending),
				errors.Is(err, coreorgs.ErrInviteExpired):
				return ErrInvalidToken
			default:
				return err
			}
		}
		return nil
	}
	if createdNew {
		if _, err := coreorgs.CreateOrgWithDefaultsInTx(ctx, w, customerID, "default", reportingTimezone); err != nil {
			return err
		}
	}
	return nil
}

// FinalizeVerifiedCustomer marks the customer's email verified.
func FinalizeVerifiedCustomer(ctx context.Context, w *dbwrite.Queries, customerID string) error {
	_, err := w.MarkCustomerEmailVerified(ctx, customerID)
	return err
}
