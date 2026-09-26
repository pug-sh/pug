package orgs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// ApplyInviteAcceptanceInTx joins the invite's org and accepts the invite. It doesn't
// check the email: a magic link is addressed to it, and other callers must.
// ErrAlreadyMember keeps the old role; treat it as success.
func ApplyInviteAcceptanceInTx(ctx context.Context, w *dbwrite.Queries, invitationID, customerID string) (string, error) {
	inv, err := w.GetOrgInvitationByIDForUpdate(ctx, invitationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invitation by id", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String() {
		return "", ErrInviteNotPending
	}
	if time.Now().After(inv.ExpiresAt.Time) {
		return "", ErrInviteExpired
	}
	inviteRole, err := ParseRole(inv.Role)
	if err != nil {
		slog.ErrorContext(ctx, "unrecognized role in org_invitations", slogx.Error(err),
			slog.String("invitation_id", inv.ID), slog.String("role", inv.Role))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	// on conflict, because a unique violation would abort the caller's transaction.
	added, err := w.CreateOrgMemberIfAbsent(ctx, dbwrite.CreateOrgMemberIfAbsentParams{
		OrgID:      inv.OrgID,
		CustomerID: customerID,
		Role:       inviteRole.String(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create org member on invite accept", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if _, err := w.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     inv.ID,
		Status: orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update invitation status", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if added == 0 {
		return inv.OrgID, ErrAlreadyMember
	}
	return inv.OrgID, nil
}
