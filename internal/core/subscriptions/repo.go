package subscriptions

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo interface {
	CreateSubscription(ctx context.Context, arg dbwrite.CreateSubscriptionParams) (dbwrite.Subscription, error)
	GetSubscription(ctx context.Context, arg dbwrite.GetSubscriptionParams) (dbwrite.Subscription, error)
	GetSubscriptionsByProject(ctx context.Context, projectID string) ([]dbread.Subscription, error)
	UpdateSubscriptionHeartbeat(ctx context.Context, arg dbwrite.UpdateSubscriptionHeartbeatParams) (dbwrite.Subscription, error)
	UpdateSubscriptionMetadata(ctx context.Context, arg dbwrite.UpdateSubscriptionMetadataParams) (dbwrite.Subscription, error)
	UpdateSubscriptionPlatform(ctx context.Context, arg dbwrite.UpdateSubscriptionPlatformParams) (dbwrite.Subscription, error)
	UpdateSubscriptionStatus(ctx context.Context, arg dbwrite.UpdateSubscriptionStatusParams) (dbwrite.Subscription, error)
	UpdateSubscriptionToken(ctx context.Context, arg dbwrite.UpdateSubscriptionTokenParams) (dbwrite.Subscription, error)
	LinkSubscriptionToUser(ctx context.Context, arg dbwrite.LinkSubscriptionToUserParams) (dbwrite.Subscription, error)
	UpdateSubscriptionUserId(ctx context.Context, arg dbwrite.UpdateSubscriptionUserIdParams) (dbwrite.Subscription, error)
}

type RepoImpl struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewRepo(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) Repo {
	return &RepoImpl{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (r *RepoImpl) CreateSubscription(ctx context.Context, arg dbwrite.CreateSubscriptionParams) (dbwrite.Subscription, error) {
	return r.write.CreateSubscription(ctx, arg)
}

func (r *RepoImpl) GetSubscription(ctx context.Context, arg dbwrite.GetSubscriptionParams) (dbwrite.Subscription, error) {
	return r.write.GetSubscription(ctx, arg)
}

func (r *RepoImpl) GetSubscriptionsByProject(ctx context.Context, projectID string) ([]dbread.Subscription, error) {
	return r.read.GetSubscriptionsByProject(ctx, projectID)
}

func (r *RepoImpl) UpdateSubscriptionHeartbeat(ctx context.Context, arg dbwrite.UpdateSubscriptionHeartbeatParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionHeartbeat(ctx, arg)
}

func (r *RepoImpl) UpdateSubscriptionMetadata(ctx context.Context, arg dbwrite.UpdateSubscriptionMetadataParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionMetadata(ctx, arg)
}

func (r *RepoImpl) UpdateSubscriptionPlatform(ctx context.Context, arg dbwrite.UpdateSubscriptionPlatformParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionPlatform(ctx, arg)
}

func (r *RepoImpl) UpdateSubscriptionStatus(ctx context.Context, arg dbwrite.UpdateSubscriptionStatusParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionStatus(ctx, arg)
}

func (r *RepoImpl) UpdateSubscriptionToken(ctx context.Context, arg dbwrite.UpdateSubscriptionTokenParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionToken(ctx, arg)
}

func (r *RepoImpl) LinkSubscriptionToUser(ctx context.Context, arg dbwrite.LinkSubscriptionToUserParams) (dbwrite.Subscription, error) {
	return r.write.LinkSubscriptionToUser(ctx, arg)
}

func (r *RepoImpl) UpdateSubscriptionUserId(ctx context.Context, arg dbwrite.UpdateSubscriptionUserIdParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionUserId(ctx, arg)
}
