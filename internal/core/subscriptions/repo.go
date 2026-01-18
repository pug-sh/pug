package subscriptions

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

func (r *repo) CreateSubscription(ctx context.Context, arg dbwrite.CreateSubscriptionParams) (dbwrite.Subscription, error) {
	return r.write.CreateSubscription(ctx, arg)
}

func (r *repo) GetSubscription(ctx context.Context, arg dbwrite.GetSubscriptionParams) (dbwrite.Subscription, error) {
	return r.write.GetSubscription(ctx, arg)
}

func (r *repo) GetSubscriptionsByProject(ctx context.Context, projectID string) ([]dbread.Subscription, error) {
	return r.read.GetSubscriptionsByProject(ctx, projectID)
}

func (r *repo) UpdateSubscriptionHeartbeat(ctx context.Context, arg dbwrite.UpdateSubscriptionHeartbeatParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionHeartbeat(ctx, arg)
}

func (r *repo) UpdateSubscriptionMetadata(ctx context.Context, arg dbwrite.UpdateSubscriptionMetadataParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionMetadata(ctx, arg)
}

func (r *repo) UpdateSubscriptionPlatform(ctx context.Context, arg dbwrite.UpdateSubscriptionPlatformParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionPlatform(ctx, arg)
}

func (r *repo) UpdateSubscriptionStatus(ctx context.Context, arg dbwrite.UpdateSubscriptionStatusParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionStatus(ctx, arg)
}

func (r *repo) UpdateSubscriptionToken(ctx context.Context, arg dbwrite.UpdateSubscriptionTokenParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionToken(ctx, arg)
}

func (r *repo) LinkSubscriptionToUser(ctx context.Context, arg dbwrite.LinkSubscriptionToUserParams) (dbwrite.Subscription, error) {
	return r.write.LinkSubscriptionToUser(ctx, arg)
}

func (r *repo) UpdateSubscriptionUserId(ctx context.Context, arg dbwrite.UpdateSubscriptionUserIdParams) (dbwrite.Subscription, error) {
	return r.write.UpdateSubscriptionUserId(ctx, arg)
}
