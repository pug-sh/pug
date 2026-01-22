package projects

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (s *Service) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) error {
	_, err := s.write.DeleteProject(ctx, arg)
	return err
}

func (s *Service) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return s.write.CreateProject(ctx, arg)
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
	return s.write.UpdateProjectDisplayName(ctx, arg)
}

func (s *Service) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	return s.write.UpdateFCMServiceJSON(ctx, arg)
}
