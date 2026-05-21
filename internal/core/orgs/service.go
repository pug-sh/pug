package orgs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/emailaction"
	"github.com/pug-sh/pug/internal/core/projects"
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

// emailPublishFailureCounter is incremented whenever an invite-email job is
// created (invitation row committed) but the subsequent NATS publish errors.
// Operators should alert on a non-zero rate: it means admins see an
// "invitation sent" 200 response for invites whose emails were never queued.
// The {kind} attribute lets ops bucket failures by payload type — for this
// package the only emitted kind is "org_member_invite".
//
// Lives in its own package (separate from auth.emailPublishFailureCounter)
// to avoid an orgs→auth import; both counters share the same metric name and
// are aggregated by the OTel collector by scope+kind.
var emailPublishFailureCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/orgs")
	// Panic on init failure: without this counter, every subsequent
	// Add() is a no-op and the only alarm for silent email drops is gone.
	// Fail loud at startup rather than silently lose the alerting signal.
	c, err := meter.Int64Counter(
		"emails.publish_failure_total",
		metric.WithDescription("Email jobs whose tx committed but NATS publish failed; indicates user-visible silent drops."),
	)
	if err != nil {
		panic("orgs: failed to register emails.publish_failure_total counter: " + err.Error())
	}
	emailPublishFailureCounter = c
}

var (
	ErrAlreadyMember        = errors.New("already a member of this org")
	ErrInviteAlreadyPending = errors.New("a pending invitation already exists for this email")
	ErrInviteExpired        = errors.New("invitation has expired")
	ErrInviteNotFound       = errors.New("invitation not found")
	ErrInviteNotPending     = errors.New("invitation is not pending")
	// ErrLastAdmin is returned by both RemoveMemberSafe and Leave when the
	// target is an admin and removing them would leave the org with zero
	// admins. In Leave, this also takes precedence over ErrLastMember when
	// the caller is an admin who is also the sole member of the org.
	// Handlers build their own client-facing message (different wording for
	// remove vs leave) — never pass this sentinel directly into
	// connect.NewError, as the neutral phrasing here is not a substitute for
	// the verb-specific message.
	ErrLastAdmin                 = errors.New("blocked: last admin of org")
	ErrLastMember                = errors.New("blocked: only member of org")
	ErrMemberNotFound            = errors.New("member not found")
	ErrOrgNotFound               = errors.New("org not found")
	ErrUnsupportedRoleTransition = errors.New("role transition not supported")
)

const (
	inviteTTL = 7 * 24 * time.Hour

	// Postgres constraint / index names used to disambiguate UniqueViolation
	// errors. Kept narrow on purpose: catching a generic "unique violation"
	// would mis-translate any future constraint added to these tables.
	//
	// These names are load-bearing: if either is renamed in a future migration
	// without updating this constant, the narrow translation silently falls
	// through and ErrAlreadyMember / ErrInviteAlreadyPending stop firing.
	// Sources:
	//   - org_members_pkey: auto-generated PK in schema/postgres/migrations/003_create_org_members.sql
	//   - org_invitations_org_email_pending: named partial index in
	//     schema/postgres/migrations/004_create_org_invitations.sql:13
	orgMembersPKey              = "org_members_pkey"
	orgInvitationsPendingUnique = "org_invitations_org_email_pending"
)

type Service struct {
	read      *dbread.Queries
	write     *dbwrite.Queries
	pgW       *pgxpool.Pool
	publisher JobPublisher
}

type JobPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// InviteDispatch bundles a persisted invitation with the raw (unhashed) token
// it was issued under. The token hash lives in email_action_tokens; the raw
// form is returned here so handlers can surface it to the inviter for
// link-sharing without re-reading the DB.
type InviteDispatch struct {
	Invitation dbwrite.OrgInvitation
	RawToken   string
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, publisher JobPublisher) *Service {
	return &Service{
		read:      dbread.New(pgRO),
		write:     dbwrite.New(pgW),
		pgW:       pgW,
		publisher: publisher,
	}
}

// CreateOrgWithDefaultsInTx performs the org + admin member + default project
// inserts inside an existing transaction. Caller owns the tx lifecycle.
// Used by auth.CompleteMagicLink (which provisions a default org for a brand-new
// passwordless account in the same tx that consumes the magic-link token) and by
// CreateOrgWithDefaults (which owns its own tx).
func CreateOrgWithDefaultsInTx(
	ctx context.Context,
	w *dbwrite.Queries,
	customerID, displayName string,
) (dbwrite.Org, error) {
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: displayName,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create org", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Org{}, err
	}

	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customerID,
		Role:       RoleAdmin.String(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to add customer as admin", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Org{}, err
	}

	if _, err := projects.CreateProjectInTx(ctx, w, org.ID, "default"); err != nil {
		// projects.CreateProjectInTx logs + records at source.
		return dbwrite.Org{}, err
	}
	return org, nil
}

// CreateOrgWithDefaults opens its own transaction around CreateOrgWithDefaultsInTx.
// Use this from RPC handlers and other callers without an active tx.
func (s *Service) CreateOrgWithDefaults(
	ctx context.Context,
	customerID, displayName string,
) (dbwrite.Org, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin CreateOrgWithDefaults tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Org{}, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back CreateOrgWithDefaults tx", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	org, err := CreateOrgWithDefaultsInTx(ctx, dbwrite.New(tx), customerID, displayName)
	if err != nil {
		return dbwrite.Org{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit CreateOrgWithDefaults tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Org{}, err
	}
	return org, nil
}

func (s *Service) CreateOrg(ctx context.Context, displayName string) (dbwrite.Org, error) {
	return s.write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: displayName,
	})
}

func (s *Service) AddMember(ctx context.Context, orgID, customerID string, role Role) (dbwrite.OrgMember, error) {
	member, err := s.write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       role.String(),
	})
	if err != nil {
		if isUniqueViolationOn(err, orgMembersPKey) {
			return dbwrite.OrgMember{}, ErrAlreadyMember
		}
		slog.ErrorContext(ctx, "failed to create org member", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbwrite.OrgMember{}, err
	}
	return member, nil
}

// isUniqueViolationOn reports whether err is a Postgres unique-violation
// against the given constraint name. Used to translate specific constraint
// collisions into typed sentinels without mis-mapping unrelated violations.
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == constraint
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

// GetMember returns one member with the joined customer fields (display_name,
// email). Reads from the write pool so callers may chain this after a write
// without risking replica lag — used by UpdateMemberRole to populate the
// response with the same shape as ListMembers.
func (s *Service) GetMember(ctx context.Context, orgID, customerID string) (dbread.GetOrgMemberByOrgIDAndCustomerIDRow, error) {
	row, err := dbread.New(s.pgW).GetOrgMemberByOrgIDAndCustomerID(ctx, dbread.GetOrgMemberByOrgIDAndCustomerIDParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.GetOrgMemberByOrgIDAndCustomerIDRow{}, ErrMemberNotFound
		}
		slog.ErrorContext(ctx, "failed to get org member", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbread.GetOrgMemberByOrgIDAndCustomerIDRow{}, err
	}
	return row, nil
}

// GetMemberRole returns the calling customer's role for the given org. The
// raw DB string is parsed through ParseRole so callers receive a validated
// Role — values that drift outside the recognized set surface as errors at
// this boundary rather than silently flowing through equality checks.
func (s *Service) GetMemberRole(ctx context.Context, orgID, customerID string) (Role, error) {
	raw, err := s.read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrMemberNotFound
		}
		slog.ErrorContext(ctx, "failed to get org member role", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	role, err := ParseRole(raw)
	if err != nil {
		slog.ErrorContext(ctx, "unrecognized role in org_members", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
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

// Leave removes the calling customer from the org. Refuses if the caller is
// the only admin (ErrLastAdmin) or the only non-admin member (ErrLastMember).
// Returns ErrMemberNotFound if the caller is not a member.
//
// The guard and delete are a single CTE-based statement for atomicity against
// concurrent Leave / RemoveMember calls. ErrLastAdmin takes precedence over
// ErrLastMember: an admin who is also the sole member must first transfer
// ownership before they can leave.
//
// Disambiguation is best-effort: when the CTE returns 0 rows, a separate
// non-transactional read on org_members determines the cause. If a concurrent
// RemoveMember (or another Leave) lands between the CTE rejection and the
// disambig read, the caller may see ErrMemberNotFound instead of the rejection
// reason that originally fired. The reverse cannot happen — a row that exists
// at disambig time was either there throughout or re-inserted, and an
// in-flight removal cannot produce a ghost ErrLastAdmin / ErrLastMember. The
// caller's atomicity invariant ("either removed or definitely still a member")
// is preserved either way — only the error code on the rejection path is racy.
func (s *Service) Leave(ctx context.Context, orgID, customerID string) error {
	n, err := s.write.DeleteOrgMemberIfNotLastAdminAndNotLastMember(
		ctx,
		dbwrite.DeleteOrgMemberIfNotLastAdminAndNotLastMemberParams{
			OrgID:      orgID,
			CustomerID: customerID,
		},
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed Leave query", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 1 {
		return nil
	}

	// 0 rows: either not a member, last admin, or only member. Disambiguate.
	// Read from write pool to avoid replica lag.
	raw, err := s.write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemberNotFound
		}
		slog.ErrorContext(ctx, "failed to disambiguate Leave block", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return err
	}
	role, err := ParseRole(raw)
	if err != nil {
		slog.ErrorContext(ctx, "unrecognized role in org_members during Leave disambig", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return err
	}

	// ErrLastAdmin takes priority: if the caller is an admin they were blocked
	// by the admin-count guard in the CTE (or by both guards simultaneously
	// when they are also the sole member). In both cases the actionable message
	// is "promote someone else before leaving."
	if role == RoleAdmin {
		return ErrLastAdmin
	}

	// Non-admin blocked → only the member-count guard could have fired.
	return ErrLastMember
}

func (s *Service) InviteMember(ctx context.Context, orgID, inviterID, email string) (InviteDispatch, error) {
	return s.InviteMemberWithRole(ctx, orgID, inviterID, email, RoleMember)
}

func (s *Service) InviteMemberWithRole(ctx context.Context, orgID, inviterID, email string, role Role) (InviteDispatch, error) {
	if !role.IsValid() {
		return InviteDispatch{}, fmt.Errorf("orgs: invalid invitation role %q", role)
	}

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
		Role:      role.String(),
		Token:     storageToken,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(inviteTTL)),
	})
	if err != nil {
		// The CTE checks org_members joined with customers by email. ErrNoRows means
		// the INSERT was skipped because the email already belongs to an org member.
		if errors.Is(err, pgx.ErrNoRows) {
			return InviteDispatch{}, ErrAlreadyMember
		}
		if isUniqueViolationOn(err, orgInvitationsPendingUnique) {
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
		Purpose:         emailaction.PurposeOrgInvite.String(),
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
		// concurrent magic-link acceptance (CompleteMagicLink) raced ahead and consumed the prior token (the row
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
		Purpose:         emailaction.PurposeOrgInvite.String(),
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

// UpdateMemberRole changes a member's role. In this scope, only the
// MEMBER -> ADMIN transition is permitted. Other transitions return
// ErrUnsupportedRoleTransition (mapped to CodeInvalidArgument at the handler).
//
// Returns ErrMemberNotFound if the target is not a member of the org.
//
// This read-modify-write is intentionally non-transactional: it is safe only
// because the lone allowed transition (MEMBER → ADMIN) is monotonic — a
// concurrent re-promotion is a no-op, a concurrent removal yields ErrNoRows
// → ErrMemberNotFound, and demotion is not permitted. If any non-monotonic
// transition is added (e.g. demote, transfer-ownership), wrap in a tx with
// SELECT ... FOR UPDATE on the org_members row.
func (s *Service) UpdateMemberRole(
	ctx context.Context,
	orgID, customerID string,
	newRole Role,
) (dbwrite.OrgMember, error) {
	rawCurrent, err := s.write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.OrgMember{}, ErrMemberNotFound
		}
		slog.ErrorContext(ctx, "failed to look up current role", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbwrite.OrgMember{}, err
	}
	current, err := ParseRole(rawCurrent)
	if err != nil {
		slog.ErrorContext(ctx, "unrecognized current role in org_members", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbwrite.OrgMember{}, err
	}

	if current != RoleMember || newRole != RoleAdmin {
		return dbwrite.OrgMember{}, ErrUnsupportedRoleTransition
	}

	updated, err := s.write.UpdateOrgMemberRole(ctx, dbwrite.UpdateOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       newRole.String(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.OrgMember{}, ErrMemberNotFound
		}
		slog.ErrorContext(ctx, "failed to update member role", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID),
			slog.String("new_role", newRole.String()))
		telemetry.RecordError(ctx, err)
		return dbwrite.OrgMember{}, err
	}
	return updated, nil
}

func (s *Service) GetOrgsWithRole(ctx context.Context, customerID string) ([]dbread.GetOrgsWithRoleByCustomerIDRow, error) {
	return s.read.GetOrgsWithRoleByCustomerID(ctx, customerID)
}

// GetOrgWithRole returns a single org plus the caller's role. Returns
// ErrOrgNotFound when there is no org-and-membership for the (org_id, customer_id)
// pair — does not distinguish "no such org" from "not a member".
func (s *Service) GetOrgWithRole(ctx context.Context, orgID, customerID string) (dbread.GetOrgWithRoleByIDAndCustomerIDRow, error) {
	row, err := s.read.GetOrgWithRoleByIDAndCustomerID(ctx, dbread.GetOrgWithRoleByIDAndCustomerIDParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.GetOrgWithRoleByIDAndCustomerIDRow{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to get org with role", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbread.GetOrgWithRoleByIDAndCustomerIDRow{}, err
	}
	return row, nil
}

func hashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
