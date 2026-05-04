package projects

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

var (
	ErrAdminRequired    = errors.New("admin role required")
	ErrProjectNotFound  = errors.New("project not found")
	ErrProjectNameTaken = errors.New("a project with this name already exists in the org")
)

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	repo  *Repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, repo *Repo) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
		repo:  repo,
	}
}

func (s *Service) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) error {
	project, err := s.write.DeleteProject(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		return err
	}
	s.invalidateProject(ctx, project)
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewPrivateKey generates a 24-char private API key: "prv_" + 20 hex chars (80 bits of entropy).
func NewPrivateKey() (string, error) {
	h, err := randomHex(10)
	if err != nil {
		return "", err
	}
	return "prv_" + h, nil
}

// NewPublicKey generates a 24-char public API key: "pub_" + 20 hex chars (80 bits of entropy).
func NewPublicKey() (string, error) {
	h, err := randomHex(10)
	if err != nil {
		return "", err
	}
	return "pub_" + h, nil
}

func (s *Service) CreateProjectAsAdmin(ctx context.Context, orgID, customerID, displayName string) (dbwrite.Project, error) {
	privKey, err := NewPrivateKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project private key", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	pubKey, err := NewPublicKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project public key", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	project, err := s.write.CreateProjectAsAdmin(ctx, dbwrite.CreateProjectAsAdminParams{
		ID:            xid.New().String(),
		PrivateApiKey: privKey,
		PublicApiKey:  pubKey,
		OrgID:         orgID,
		CustomerID:    customerID,
		DisplayName:   displayName,
	})
	if err != nil {
		// The CTE checks org_members for admin role. ErrNoRows means the INSERT was
		// skipped because no admin row exists for this org_id + customer_id.
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrAdminRequired
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return dbwrite.Project{}, ErrProjectNameTaken
		}
		slog.ErrorContext(ctx, "failed to create project as admin", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	return project, nil
}

func (s *Service) CreateProject(ctx context.Context, orgID, displayName string) (dbwrite.Project, error) {
	privKey, err := NewPrivateKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project private key", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	pubKey, err := NewPublicKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project public key", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	project, err := s.write.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:            xid.New().String(),
		PrivateApiKey: privKey,
		PublicApiKey:  pubKey,
		OrgID:         orgID,
		DisplayName:   displayName,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return dbwrite.Project{}, ErrProjectNameTaken
		}
		slog.ErrorContext(ctx, "failed to create project", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	return project, nil
}

func (s *Service) GetProjectByID(ctx context.Context, id string) (dbread.Project, error) {
	return s.read.GetProjectByID(ctx, id)
}

func (s *Service) GetProjectsByOrgID(ctx context.Context, orgID string) ([]dbread.Project, error) {
	return s.read.GetProjectsByOrgID(ctx, orgID)
}

func (s *Service) ProjectExistsForOrgMember(ctx context.Context, projectID string, customerID string) (bool, error) {
	if _, err := s.read.GetProjectByIDAndOrgMember(ctx, dbread.GetProjectByIDAndOrgMemberParams{
		ID:         projectID,
		CustomerID: customerID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Service) UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateProjectDisplayName(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrProjectNotFound
		}
		return dbwrite.Project{}, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateFCMServiceJSON(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrProjectNotFound
		}
		return dbwrite.Project{}, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) invalidateProject(ctx context.Context, project dbwrite.Project) {
	if s.repo == nil {
		slog.WarnContext(ctx, "cache repo not set; skipping project cache invalidation", slog.String("project_id", project.ID))
		return
	}
	s.repo.InvalidateProjectKeys(ctx, project.PrivateApiKey, project.PublicApiKey)
}
