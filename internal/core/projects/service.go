package projects

import (
	"context"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
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
		return err
	}
	s.invalidateProject(ctx, project)
	return nil
}

func newPrivateKey() string { return "prv_" + xid.New().String() }
func newPublicKey() string  { return "pub_" + xid.New().String() }

func (s *Service) CreateProject(ctx context.Context, customerID, displayName string) (dbwrite.Project, error) {
	return s.write.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:            xid.New().String(),
		PrivateApiKey: newPrivateKey(),
		PublicApiKey:  newPublicKey(),
		CustomerID:    customerID,
		DisplayName:   displayName,
	})
}

func (s *Service) GetProjectByID(ctx context.Context, id string) (dbread.Project, error) {
	return s.read.GetProjectByID(ctx, id)
}

func (s *Service) GetProjectsByCustomerID(ctx context.Context, customerID string) ([]dbread.Project, error) {
	return s.read.GetProjectsByCustomerID(ctx, customerID)
}

func (s *Service) ProjectExistsForCustomer(ctx context.Context, projectID string, customerID string) (bool, error) {
	return s.read.ProjectExistsForCustomer(ctx, dbread.ProjectExistsForCustomerParams{
		ID:         projectID,
		CustomerID: customerID,
	})
}

func (s *Service) UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateProjectDisplayName(ctx, arg)
	if err != nil {
		return project, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateFCMServiceJSON(ctx, arg)
	if err != nil {
		return project, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) invalidateProject(ctx context.Context, project dbwrite.Project) {
	if s.repo == nil {
		slog.WarnContext(ctx, "cache repo not set; skipping project cache invalidation", slog.String("projectID", project.ID))
		return
	}
	s.repo.InvalidateProjectKeys(ctx, project.PrivateApiKey, project.PublicApiKey)
}
