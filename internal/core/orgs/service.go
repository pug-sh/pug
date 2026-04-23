package orgs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	orgsv1 "github.com/fivebitsio/cotton/internal/gen/proto/dashboard/orgs/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

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
	inviteTTL = 7 * 24 * time.Hour
)

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	pgW   *pgxpool.Pool
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
		pgW:   pgW,
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
			return err
		}
		return ErrLastAdmin
	}
	return nil
}

func (s *Service) InviteMember(ctx context.Context, orgID, inviterID, email string) (dbwrite.OrgInvitation, error) {
	token, err := newInviteToken()
	if err != nil {
		return dbwrite.OrgInvitation{}, fmt.Errorf("generate invite token: %w", err)
	}
	inv, err := s.write.CreateOrgInvitation(ctx, dbwrite.CreateOrgInvitationParams{
		ID:        xid.New().String(),
		OrgID:     orgID,
		InviterID: postgres.NewOptionalText(inviterID),
		Email:     email,
		Token:     token,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(inviteTTL)),
	})
	if err != nil {
		// The CTE checks org_members joined with customers by email. ErrNoRows means
		// the INSERT was skipped because the email already belongs to an org member.
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.OrgInvitation{}, ErrAlreadyMember
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return dbwrite.OrgInvitation{}, ErrInviteAlreadyPending
		}
		return dbwrite.OrgInvitation{}, err
	}
	return inv, nil
}

func (s *Service) AcceptInvite(ctx context.Context, token, customerID, customerEmail string) (dbread.Org, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin accept invite transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)

	inv, err := w.GetOrgInvitationByTokenForUpdate(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.Org{}, ErrInviteNotFound
		}
		slog.ErrorContext(ctx, "failed to get org invitation by token", slogx.Error(err))
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

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit accept invite transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbread.Org{}, err
	}

	// Read from write pool to avoid read-replica lag after the commit.
	wOrg, err := s.write.GetOrgByID(ctx, inv.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch org after accepting invite", slogx.Error(err), slog.String("orgID", inv.OrgID))
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
