package journeys

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo interface {
	CreateJourney(ctx context.Context, arg dbwrite.CreateJourneyParams) (dbwrite.Journey, error)
	GetJourneysByProjectID(ctx context.Context, projectID string) ([]dbread.Journey, error)
}

type repoImpl struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewRepo(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) Repo {
	return &repoImpl{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (r *repoImpl) CreateJourney(ctx context.Context, arg dbwrite.CreateJourneyParams) (dbwrite.Journey, error) {
	return r.write.CreateJourney(ctx, arg)
}

func (r *repoImpl) GetJourneysByProjectID(ctx context.Context, projectID string) ([]dbread.Journey, error) {
	return r.read.GetJourneysByProjectID(ctx, projectID)
}
