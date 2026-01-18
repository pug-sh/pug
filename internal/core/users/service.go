package users

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	repo repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		repo: *newRepo(pgRO, pgW),
	}
}

func (s *Service) GetUserByID(ctx context.Context, id string) (dbread.User, error) {
	return s.repo.GetUserByID(ctx, id)
}

func (s *Service) GetUserByProjectAndExternalID(ctx context.Context, arg dbread.GetUserByProjectAndExternalIDParams) (dbread.User, error) {
	return s.repo.GetUserByProjectAndExternalID(ctx, arg)
}

func (s *Service) GetUsersByProjectID(ctx context.Context, id string) ([]dbread.User, error) {
	return s.repo.GetUsersByProjectID(ctx, id)
}

func (s *Service) CreateUser(ctx context.Context, arg dbwrite.CreateUserParams) (dbwrite.User, error) {
	return s.repo.CreateUser(ctx, arg)
}

func (s *Service) UpdateUserProperties(ctx context.Context, arg dbwrite.UpdateUserPropertiesParams) (dbwrite.User, error) {
	return s.repo.UpdateUserProperties(ctx, arg)
}

func (s *Service) UpdateUserCustomProperties(ctx context.Context, arg dbwrite.UpdateUserCustomPropertiesParams) (dbwrite.User, error) {
	return s.repo.UpdateUserCustomProperties(ctx, arg)
}

func (s *Service) DeleteUserByID(ctx context.Context, id string) error {
	return s.repo.DeleteUserByID(ctx, id)
}
