package orgs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
)

// Mirror of auth.emailPublishFailureCounter — see that declaration for the
// alerting rationale. Lives in its own package to avoid an orgs→auth import.
var emailPublishFailureCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/orgs")
	emailPublishFailureCounter, _ = meter.Int64Counter(
		"emails.publish_failure_total",
		metric.WithDescription("Email jobs whose tx committed but NATS publish failed; indicates user-visible silent drops."),
	)
}

var (
	ErrAlreadyMember        = errors.New("already a member of this org")
	ErrInviteAlreadyPending = errors.New("a pending invitation already exists for this email")
	ErrInviteExpired        = errors.New("invitation has expired")
	ErrInviteNotFound       = errors.New("invitation not found")
	ErrInviteNotPending     = errors.New("invitation is not pending")
	ErrInviteWrongEmail     = errors.New("invitation was issued to a different email address")
	ErrLastAdmin            = errors.New("cannot remove the last admin")
	ErrMemberNotFound       = errors.New("member not found")
	ErrOrgNotFound          = errors.New("org not found")
)

const (
	inviteTTL        = 7 * 24 * time.Hour
	orgInvitePurpose = "org_invite"
)

type Service struct {
	read      *dbread.Queries
	write     *dbwrite.Queries
	pgW       *pgxpool.Pool
	publisher jobPublisher
}

type jobPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

type InviteDispatch struct {
	Invitation dbwrite.OrgInvitation
	RawToken   string
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, publisher jobPublisher) *Service {
	return &Service{
		read:      dbread.New(pgRO),
		write:     dbwrite.New(pgW),
		pgW:       pgW,
		publisher: publisher,
	}
}

func (s *Service) CreateOrg(ctx context.Context, displayName string) (dbwrite.Org, error) {
	return s.write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: displayName,
	})
}

func (s *Service) AddMember(ctx context.Context, orgID, customerID, role string) (dbwrite.OrgMember, error) {
	member, err := s.write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       role,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return dbwrite.OrgMember{}, ErrAlreadyMember
		}
		return dbwrite.OrgMember{}, err
	}
	return member, nil
}

func (s *Service) GetOrgByID(ctx context.Context, id string) (dbread.Org, error) {
	org, err := s.read.GetOrgByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.Org{}, ErrOrgNotFound
		}
		return dbread.Org{}, err
	}
	return org, nil
}

func (s *Service) GetOrgsByCustomerID(ctx context.Context, customerID string) ([]dbread.Org, error) {
	return s.read.GetOrgsByCustomerID(ctx, customerID)
}

func (s *Service) UpdateDisplayName(ctx context.Context, id, displayName string) (dbwrite.Org, error) {
	org, err := s.write.UpdateOrgDisplayName(ctx, dbwrite.UpdateOrgDisplayNameParams{
		ID:          id,
		DisplayName: displayName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Org{}, ErrOrgNotFound
		}
		return dbwrite.Org{}, err
	}
	return org, nil
}

func (s *Service) IsOrgMember(ctx context.Context, orgID, customerID string) (bool, error) {
	return s.read.IsOrgMember(ctx, dbread.IsOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
}

func (s *Service) ListMembers(ctx context.Context, orgID string) ([]dbread.GetOrgMembersByOrgIDRow, error) {
	return s.read.GetOrgMembersByOrgID(ctx, orgID)
}

func (s *Service) GetMemberRole(ctx context.Context, orgID, customerID string) (string, error) {
	role, err := s.read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrMemberNotFound
		}
		return "", err
	}
	return role, nil
}

// RemoveMemberSafe atomically deletes a member, refusing to remove the last admin.
// Returns ErrMemberNotFound if the member does not exist, or ErrLastAdmin if the
// delete was blocked because the target is the only admin.
func (s *Service) RemoveMemberSafe(ctx context.Context, orgID, customerID string) error {
	n, err := s.write.DeleteOrgMemberIfNotLastAdmin(ctx, dbwrite.DeleteOrgMemberIfNotLastAdminParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to delete org member", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
		// Distinguish "member not found" from "last admin blocked": check if
		// the member still exists. Use write pool to avoid read-replica lag.
		if _, err := s.write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{
			OrgID:      orgID,
			CustomerID: customerID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMemberNotFound
			}
			slog.ErrorContext(ctx, "failed to disambiguate last-admin block from missing member",
				slogx.Error(err),
				slog.String("org_id", orgID), slog.String("customer_id", customerID))
			telemetry.RecordError(ctx, err)
			return err
		}
		return ErrLastAdmin
	}
	return nil
}

func (s *Service) InviteMember(ctx context.Context, orgID, inviterID, email string) (InviteDispatch, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin invite member transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back invite member transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	w := dbwrite.New(tx)
	storageToken, err := newInviteToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate invite storage token", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, fmt.Errorf("generate invite storage token: %w", err)
	}
	inv, err := w.CreateOrgInvitation(ctx, dbwrite.CreateOrgInvitationParams{
		ID:        xid.New().String(),
		OrgID:     orgID,
		InviterID: postgres.NewOptionalText(inviterID),
		Email:     email,
		Token:     storageToken,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(inviteTTL)),
	})
	if err != nil {
		// The CTE checks org_members joined with customers by email. ErrNoRows means
		// the INSERT was skipped because the email already belongs to an org member.
		if errors.Is(err, pgx.ErrNoRows) {
			return InviteDispatch{}, ErrAlreadyMember
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return InviteDispatch{}, ErrInviteAlreadyPending
		}
		slog.ErrorContext(ctx, "failed to create org invitation", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}

	rawToken, err := s.issueInviteEmailToken(ctx, w, inv)
	if err != nil {
		return InviteDispatch{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit invite member transaction", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}

	s.publishInviteEmailJob(ctx, inv, rawToken)
	return InviteDispatch{Invitation: inv, RawToken: rawToken}, nil
}

func (s *Service) ResendInvite(ctx context.Context, orgID, invitationID string) (InviteDispatch, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin resend invite transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back resend invite transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	w := dbwrite.New(tx)
	inv, err := w.GetOrgInvitationByIDForUpdate(ctx, invitationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InviteDispatch{}, ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invitation for resend", slogx.Error(err), slog.String("invitation_id", invitationID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}
	if inv.OrgID != orgID {
		return InviteDispatch{}, ErrInviteNotFound
	}
	if inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String() {
		return InviteDispatch{}, ErrInviteNotPending
	}

	storageToken, err := newInviteToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate invite storage token", slogx.Error(err),
			slog.String("invitation_id", invitationID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, fmt.Errorf("generate invite storage token: %w", err)
	}

	inv, err = w.RefreshOrgInvitationDelivery(ctx, dbwrite.RefreshOrgInvitationDeliveryParams{
		ID:        inv.ID,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(inviteTTL)),
		Token:     storageToken,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to refresh org invitation delivery", slogx.Error(err), slog.String("invitation_id", invitationID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}

	n, err := w.InvalidateActiveEmailActionTokensByInvitation(ctx, dbwrite.InvalidateActiveEmailActionTokensByInvitationParams{
		OrgInvitationID: postgres.NewOptionalText(inv.ID),
		Purpose:         orgInvitePurpose,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to invalidate org invite tokens", slogx.Error(err), slog.String("invitation_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}
	if n == 0 {
		// Zero rows is benign in two cases: the invitation aged past inviteTTL while
		// staying PENDING (the prior token's expires_at is now in the past, so the
		// `expires_at > now()` filter on InvalidateActive skips it — see the
		// resurrect-expired-invite flow in TestResendInviteExtendsExpiresAt), or a
		// concurrent AcceptInvite raced ahead and consumed the prior token (the row
		// lock on org_invitations serializes them but a marked-consumed row is still
		// filtered out here). A genuine invariant violation — PENDING invitation with
		// no token ever issued — would also land here. Surface for investigation
		// without failing the resend; the freshly-issued token below restores the
		// invariant in every case.
		slog.WarnContext(ctx, "resend invalidated no prior invite tokens",
			slog.String("invitation_id", inv.ID))
	}

	rawToken, err := s.issueInviteEmailToken(ctx, w, inv)
	if err != nil {
		return InviteDispatch{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit resend invite transaction", slogx.Error(err), slog.String("invitation_id", invitationID))
		telemetry.RecordError(ctx, err)
		return InviteDispatch{}, err
	}

	s.publishInviteEmailJob(ctx, inv, rawToken)
	return InviteDispatch{Invitation: inv, RawToken: rawToken}, nil
}

func (s *Service) AcceptInvite(ctx context.Context, token, customerID, customerEmail string) (dbread.Org, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin accept invite transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back accept invite transaction", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	emailToken, err := r.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashInviteToken(token),
		Purpose:   orgInvitePurpose,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.Org{}, ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invite token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}
	if !emailToken.OrgInvitationID.Valid {
		return dbread.Org{}, ErrInviteNotFound
	}

	inv, err := w.GetOrgInvitationByIDForUpdate(ctx, emailToken.OrgInvitationID.String)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.Org{}, ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invitation by id", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	if !strings.EqualFold(inv.Email, customerEmail) {
		return dbread.Org{}, ErrInviteWrongEmail
	}
	if inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String() {
		return dbread.Org{}, ErrInviteNotPending
	}
	if time.Now().After(inv.ExpiresAt.Time) {
		return dbread.Org{}, ErrInviteExpired
	}

	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      inv.OrgID,
		CustomerID: customerID,
		Role:       orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return dbread.Org{}, ErrAlreadyMember
		}
		slog.ErrorContext(ctx, "failed to create org member on invite accept", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	if _, err := w.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     inv.ID,
		Status: orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update invitation status", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	// ErrNoRows here is safe to ignore: the row lock taken by
	// GetOrgInvitationByIDForUpdate above serializes us against any concurrent
	// ResendInvite, so the token can only become consumed/expired strictly
	// before or strictly after this tx — never mid-flight. A missing row at
	// this point means the token expired between GetValid and now (a sub-
	// second window) and the invitation's expiry guard above should have
	// already rejected the request.
	if _, err := w.ConsumeEmailActionToken(ctx, emailToken.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to consume org invite token", slogx.Error(err), slog.String("token_id", emailToken.ID))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit accept invite transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	// Read from write pool to avoid read-replica lag after the commit.
	wOrg, err := s.write.GetOrgByID(ctx, inv.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch org after accepting invite", slogx.Error(err), slog.String("org_id", inv.OrgID))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}
	return dbread.Org{
		ID:          wOrg.ID,
		DisplayName: wOrg.DisplayName,
		CreateTime:  wOrg.CreateTime,
		UpdateTime:  wOrg.UpdateTime,
	}, nil
}

func (s *Service) ListInvitations(ctx context.Context, orgID string) ([]dbread.OrgInvitation, error) {
	return s.read.GetOrgInvitationsByOrgID(ctx, orgID)
}

func newInviteToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) issueInviteEmailToken(ctx context.Context, w *dbwrite.Queries, inv dbwrite.OrgInvitation) (string, error) {
	rawToken, err := newInviteToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate invite token", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invitation_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return "", fmt.Errorf("generate invite token: %w", err)
	}
	if _, err := w.CreateEmailActionToken(ctx, dbwrite.CreateEmailActionTokenParams{
		ID:              xid.New().String(),
		CustomerID:      postgres.NewOptionalText(""),
		Email:           inv.Email,
		Purpose:         orgInvitePurpose,
		TokenHash:       hashInviteToken(rawToken),
		OrgInvitationID: postgres.NewOptionalText(inv.ID),
		ExpiresAt:       postgres.NewTimestamptz(inv.ExpiresAt.Time),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create org invite email token", slogx.Error(err), slog.String("org_id", inv.OrgID), slog.String("invitation_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return rawToken, nil
}

// publishInviteEmailJob is best-effort: a NATS failure is recorded via
// emails.publish_failure_total{kind=org_member_invite} but does NOT fail the
// calling RPC. The invitation row and its email_action_token are durable, so
// an admin can click Resend to re-trigger delivery if the metric fires.
func (s *Service) publishInviteEmailJob(ctx context.Context, inv dbwrite.OrgInvitation, token string) {
	if s.publisher == nil {
		return
	}
	data, err := proto.Marshal(&emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_OrgMemberInvite{
			OrgMemberInvite: &emailworkerv1.OrgMemberInvitePayload{
				Email:        proto.String(inv.Email),
				InvitationId: proto.String(inv.ID),
				Token:        proto.String(token),
			},
		},
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal org invite email job", slogx.Error(err), slog.String("invitation_id", inv.ID))
		telemetry.RecordError(ctx, err)
		emailPublishFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "org_member_invite")))
		return
	}
	if err := s.publisher.Publish(ctx, nats.MiscEmailJobsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish org invite email job", slogx.Error(err), slog.String("invitation_id", inv.ID))
		telemetry.RecordError(ctx, err)
		emailPublishFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "org_member_invite")))
	}
}

func hashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
