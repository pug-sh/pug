package campaigns

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	repo Repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		repo: NewRepo(pgRO, pgW),
	}
}

func (s *Service) CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	return s.repo.CreateCampaign(ctx, arg)
}

func (s *Service) GetCampaignById(ctx context.Context, id string) (dbread.Campaign, error) {
	return s.repo.GetCampaignById(ctx, id)
}

func (s *Service) GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error) {
	return s.repo.GetCampaignsByProjectID(ctx, projectID)
}

func (s *Service) DeleteCampaign(ctx context.Context, id string, projectID string) error {
	return s.repo.DeleteCampaign(ctx, dbwrite.DeleteCampaignParams{
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	return s.repo.UpdateCampaign(ctx, arg)
}
