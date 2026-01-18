package projects

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	repo *repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		repo: newRepo(pgRO, pgW),
	}
}

// DeleteProject deletes a project by its ID for the authenticated customer
func (s *Service) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) error {
	_, err := s.repo.DeleteProject(ctx, arg)
	return err
}

// CreateProject creates a new project
func (s *Service) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return s.repo.CreateProject(ctx, arg)
}

// GetProjectById retrieves a project by its ID
func (s *Service) GetProjectById(ctx context.Context, id string) (dbread.Project, error) {
	return s.repo.GetProjectById(ctx, id)
}

// GetProjectsByCustomerId retrieves all projects for a customer
func (s *Service) GetProjectsByCustomerId(ctx context.Context, customerID string) ([]dbread.Project, error) {
	return s.repo.GetProjectsByCustomerId(ctx, customerID)
}

// ProjectExistsForCustomer checks if a project exists for a given customer
func (s *Service) ProjectExistsForCustomer(ctx context.Context, projectID string, customerID string) (bool, error) {
	return s.repo.ProjectExistsForCustomer(ctx, dbread.ProjectExistsForCustomerParams{
		ID:         projectID,
		CustomerID: customerID,
	})
}

// UpdateProjectDisplayName updates the display name of a project
func (s *Service) UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error) {
	return s.repo.UpdateProjectDisplayName(ctx, arg)
}

// UpdateFCMServiceJSON updates the FCM service JSON for a project
func (s *Service) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	return s.repo.UpdateFCMServiceJSON(ctx, arg)
}
