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

// InviteTokenPurpose is the email_action_tokens.purpose for org-invite tokens.
// It is deliberately distinct from the auth package's "magic_link" login
// purpose: issuing or superseding a plain login link invalidates active tokens
// by (email, purpose), so a separate purpose guarantees that flow can never
// consume a pending invite token. CompleteMagicLink honors both purposes — only
// the invalidation scope differs by purpose.
const InviteTokenPurpose = "org_invite"

// ApplyInviteAcceptanceInTx adds customerID to the invitation's org with the
// invitation's role and flips the invitation to ACCEPTED. The caller owns the tx
// and is responsible for resolving and consuming the magic-link token that
// carried this invitation; this function does neither, and does NOT check an
// email match (the magic link is, by construction, addressed to the invited
// email). Returns ErrInviteNotFound / ErrInviteNotPending / ErrInviteExpired /
// ErrAlreadyMember for client-input outcomes; other errors are logged + recorded
// at source. ErrAlreadyMember still flips the invitation to ACCEPTED (the
// membership exists, so the invite is resolved, not pending) — the caller
// should treat it as an idempotent success.
func ApplyInviteAcceptanceInTx(ctx context.Context, w *dbwrite.Queries, invitationID, customerID string) error {
	inv, err := w.GetOrgInvitationByIDForUpdate(ctx, invitationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invitation by id", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String() {
		return ErrInviteNotPending
	}
	if time.Now().After(inv.ExpiresAt.Time) {
		return ErrInviteExpired
	}
	inviteRole, err := ParseRole(inv.Role)
	if err != nil {
		slog.ErrorContext(ctx, "unrecognized role in org_invitations", slogx.Error(err),
			slog.String("invitation_id", inv.ID), slog.String("role", inv.Role))
		telemetry.RecordError(ctx, err)
		return err
	}
	alreadyMember := false
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      inv.OrgID,
		CustomerID: customerID,
		Role:       inviteRole.String(),
	}); err != nil {
		if !isUniqueViolationOn(err, orgMembersPKey) {
			slog.ErrorContext(ctx, "failed to create org member on invite accept", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return err
		}
		// Already a member: no new row created, but the invitation is still
		// resolved — fall through to flip it to ACCEPTED so it never lingers as
		// a PENDING invitation whose token has been consumed.
		alreadyMember = true
	}
	if _, err := w.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     inv.ID,
		Status: orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update invitation status", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if alreadyMember {
		return ErrAlreadyMember
	}
	return nil
}
