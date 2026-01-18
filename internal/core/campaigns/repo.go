package campaigns

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type repo struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func newRepo(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *repo {
	return &repo{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (r *repo) CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	return r.write.CreateCampaign(ctx, arg)
}

func (r *repo) GetCampaignById(ctx context.Context, id string) (dbread.Campaign, error) {
	return r.read.GetCampaignById(ctx, id)
}

func (r *repo) GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error) {
	return r.read.GetCampaignsByProjectID(ctx, projectID)
}

func (r *repo) GetScheduledCampaigns(ctx context.Context) ([]dbread.Campaign, error) {
	return r.read.GetScheduledCampaigns(ctx)
}

func (r *repo) DeleteCampaign(ctx context.Context, arg dbwrite.DeleteCampaignParams) error {
	return r.write.DeleteCampaign(ctx, arg)
}

func (r *repo) UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaign(ctx, arg)
}

func (r *repo) UpdateCampaignStatus(ctx context.Context, arg dbwrite.UpdateCampaignStatusParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignStatus(ctx, arg)
}

func (r *repo) UpdateCampaignStartTime(ctx context.Context, arg dbwrite.UpdateCampaignStartTimeParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignStartTime(ctx, arg)
}

func (r *repo) UpdateCampaignEndTime(ctx context.Context, arg dbwrite.UpdateCampaignEndTimeParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignEndTime(ctx, arg)
}
