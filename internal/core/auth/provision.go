package auth

import (
	"context"
	"errors"
	"log/slog"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// Signup describes the account a sign-in resolved to.
type Signup struct {
	CustomerID      string
	CreatedNew      bool
	OrgInvitationID string
	ProvenDomain    string
	// ReportingTimezone seeds a new default project (coerced to UTC if malformed).
	ReportingTimezone string
}

// FinishSignup accepts the invite and runs auto-join, at sign-in and at session refresh.
// A new account that joined nothing gets a default org if its domain allows it. It
// returns the orgs joined.
func FinishSignup(ctx context.Context, w *dbwrite.Queries, s Signup) ([]string, error) {
	var joined []string
	if s.OrgInvitationID != "" {
		orgID, err := coreorgs.ApplyInviteAcceptanceInTx(ctx, w, s.OrgInvitationID, s.CustomerID)
		switch {
		case err == nil:
			joined = append(joined, orgID)
		case errors.Is(err, coreorgs.ErrAlreadyMember):
		case errors.Is(err, coreorgs.ErrInviteNotFound),
			errors.Is(err, coreorgs.ErrInviteNotPending),
			errors.Is(err, coreorgs.ErrInviteExpired):
			return nil, ErrInvalidToken
		default:
			return nil, err
		}
	}
	if s.ProvenDomain == "" && !s.CreatedNew {
		return joined, nil
	}

	email, err := w.GetCustomerEmailByID(ctx, s.CustomerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get customer email for signup", slogx.Error(err), slog.String("customer_id", s.CustomerID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	autoJoined, err := coreorgs.AutoJoinInTx(ctx, w, s.CustomerID, email, s.ProvenDomain)
	if err != nil {
		return nil, err
	}
	joined = append(joined, autoJoined...)
	if !s.CreatedNew || len(joined) > 0 {
		return joined, nil
	}

	allowed, err := coreorgs.OrgCreationAllowedInTx(ctx, w, s.CustomerID, email)
	if err != nil || !allowed {
		return nil, err
	}
	if _, err := coreorgs.CreateOrgWithDefaultsInTx(ctx, w, s.CustomerID, "default", s.ReportingTimezone); err != nil {
		return nil, err
	}
	return nil, nil
}

// FinalizeVerifiedCustomer marks the customer's email verified.
func FinalizeVerifiedCustomer(ctx context.Context, w *dbwrite.Queries, customerID string) error {
	_, err := w.MarkCustomerEmailVerified(ctx, customerID)
	return err
}
