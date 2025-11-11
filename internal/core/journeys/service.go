package journeys

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

func (s *Service) CreateJourney(ctx context.Context, arg dbwrite.CreateJourneyParams) (dbwrite.Journey, error) {
	return s.repo.CreateJourney(ctx, arg)
}

func (s *Service) GetJourneysByProjectID(ctx context.Context, projectID string) ([]dbread.Journey, error) {
	return s.repo.GetJourneysByProjectID(ctx, projectID)
}
