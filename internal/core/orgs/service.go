package orgs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

var (
	ErrAlreadyMember    = errors.New("already a member of this org")
	ErrInviteExpired    = errors.New("invitation has expired")
	ErrInviteNotPending = errors.New("invitation is not pending")
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"

	InviteStatusPending  = "pending"
	InviteStatusAccepted = "accepted"

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
	return s.write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       role,
	})
}

func (s *Service) GetOrgByID(ctx context.Context, id string) (dbread.Org, error) {
	return s.read.GetOrgByID(ctx, id)
}

func (s *Service) GetOrgsByCustomerID(ctx context.Context, customerID string) ([]dbread.Org, error) {
	return s.read.GetOrgsByCustomerID(ctx, customerID)
}

func (s *Service) UpdateDisplayName(ctx context.Context, id, displayName string) (dbwrite.Org, error) {
	return s.write.UpdateOrgDisplayName(ctx, dbwrite.UpdateOrgDisplayNameParams{
		ID:          id,
		DisplayName: displayName,
	})
}

func (s *Service) IsOrgMember(ctx context.Context, orgID, customerID string) (bool, error) {
	return s.read.IsOrgMember(ctx, dbread.IsOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
}

func (s *Service) IsOrgAdmin(ctx context.Context, orgID, customerID string) (bool, error) {
	role, err := s.read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return role == RoleAdmin, nil
}

func (s *Service) ListMembers(ctx context.Context, orgID string) ([]dbread.GetOrgMembersByOrgIDRow, error) {
	return s.read.GetOrgMembersByOrgID(ctx, orgID)
}

func (s *Service) RemoveMember(ctx context.Context, orgID, customerID string) error {
	return s.write.DeleteOrgMember(ctx, dbwrite.DeleteOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
	})
}

func (s *Service) InviteMember(ctx context.Context, orgID, inviterID, email string) (dbwrite.OrgInvitation, error) {
	token, err := newInviteToken()
	if err != nil {
		return dbwrite.OrgInvitation{}, fmt.Errorf("generate invite token: %w", err)
	}
	return s.write.CreateOrgInvitation(ctx, dbwrite.CreateOrgInvitationParams{
		ID:        xid.New().String(),
		OrgID:     orgID,
		InviterID: inviterID,
		Email:     email,
		Token:     token,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(inviteTTL), Valid: true},
	})
}

func (s *Service) AcceptInvite(ctx context.Context, token, customerID string) (dbread.Org, error) {
	inv, err := s.read.GetOrgInvitationByToken(ctx, token)
	if err != nil {
		return dbread.Org{}, err
	}

	if inv.Status != InviteStatusPending {
		return dbread.Org{}, ErrInviteNotPending
	}
	if time.Now().After(inv.ExpiresAt.Time) {
		return dbread.Org{}, ErrInviteExpired
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin accept invite transaction", slogx.Error(err))
		return dbread.Org{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)

	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      inv.OrgID,
		CustomerID: customerID,
		Role:       RoleMember,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return dbread.Org{}, ErrAlreadyMember
		}
		slog.ErrorContext(ctx, "failed to create org member on invite accept", slogx.Error(err))
		return dbread.Org{}, err
	}

	if _, err := w.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     inv.ID,
		Status: InviteStatusAccepted,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update invitation status", slogx.Error(err))
		return dbread.Org{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit accept invite transaction", slogx.Error(err))
		return dbread.Org{}, err
	}

	org, err := s.read.GetOrgByID(ctx, inv.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch org after accepting invite", slogx.Error(err), slog.String("orgID", inv.OrgID))
		return dbread.Org{}, err
	}
	return org, nil
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
