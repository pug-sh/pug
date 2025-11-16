package campaigns

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo interface {
	CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error)
	GetCampaignById(ctx context.Context, id string) (dbread.Campaign, error)
	GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error)
	GetScheduledCampaigns(ctx context.Context) ([]dbread.Campaign, error)
	DeleteCampaign(ctx context.Context, arg dbwrite.DeleteCampaignParams) error
	UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error)
	UpdateCampaignStatus(ctx context.Context, arg dbwrite.UpdateCampaignStatusParams) (dbwrite.Campaign, error)
	UpdateCampaignStartTime(ctx context.Context, arg dbwrite.UpdateCampaignStartTimeParams) (dbwrite.Campaign, error)
	UpdateCampaignEndTime(ctx context.Context, arg dbwrite.UpdateCampaignEndTimeParams) (dbwrite.Campaign, error)
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

func (r *repoImpl) CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	return r.write.CreateCampaign(ctx, arg)
}

func (r *repoImpl) GetCampaignById(ctx context.Context, id string) (dbread.Campaign, error) {
	return r.read.GetCampaignById(ctx, id)
}

func (r *repoImpl) GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error) {
	return r.read.GetCampaignsByProjectID(ctx, projectID)
}

func (r *repoImpl) GetScheduledCampaigns(ctx context.Context) ([]dbread.Campaign, error) {
	return r.read.GetScheduledCampaigns(ctx)
}

func (r *repoImpl) DeleteCampaign(ctx context.Context, arg dbwrite.DeleteCampaignParams) error {
	return r.write.DeleteCampaign(ctx, arg)
}

func (r *repoImpl) UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaign(ctx, arg)
}

func (r *repoImpl) UpdateCampaignStatus(ctx context.Context, arg dbwrite.UpdateCampaignStatusParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignStatus(ctx, arg)
}

func (r *repoImpl) UpdateCampaignStartTime(ctx context.Context, arg dbwrite.UpdateCampaignStartTimeParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignStartTime(ctx, arg)
}

func (r *repoImpl) UpdateCampaignEndTime(ctx context.Context, arg dbwrite.UpdateCampaignEndTimeParams) (dbwrite.Campaign, error) {
	return r.write.UpdateCampaignEndTime(ctx, arg)
}
