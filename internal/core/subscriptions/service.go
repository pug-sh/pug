package subscriptions

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

type Service struct {
	repo *repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		repo: newRepo(pgRO, pgW),
	}
}

func (s *Service) CreateSubscription(ctx context.Context, projectID, token, platform string, metadata []byte, status string) (dbwrite.Subscription, error) {
	params := dbwrite.CreateSubscriptionParams{
		ID:        xid.New().String(),
		ProjectID: projectID,
		Token:     token,
		Platform:  platform,
		Metadata:  metadata,
		Status:    status,
	}
	return s.repo.CreateSubscription(ctx, params)
}

func (s *Service) GetSubscription(ctx context.Context, id, projectID string) (dbwrite.Subscription, error) {
	params := dbwrite.GetSubscriptionParams{
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.GetSubscription(ctx, params)
}

func (s *Service) UpdateSubscriptionHeartbeat(ctx context.Context, id, projectID string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionHeartbeatParams{
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.UpdateSubscriptionHeartbeat(ctx, params)
}

func (s *Service) UpdateSubscriptionMetadata(ctx context.Context, id, projectID string, metadata []byte) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionMetadataParams{
		Metadata:  metadata,
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.UpdateSubscriptionMetadata(ctx, params)
}

func (s *Service) UpdateSubscriptionPlatform(ctx context.Context, id, projectID, platform string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionPlatformParams{
		Platform:  platform,
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.UpdateSubscriptionPlatform(ctx, params)
}

func (s *Service) UpdateSubscriptionStatus(ctx context.Context, id, projectID, status string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionStatusParams{
		Status:    status,
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.UpdateSubscriptionStatus(ctx, params)
}

func (s *Service) UpdateSubscriptionToken(ctx context.Context, id, projectID, token string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionTokenParams{
		Token:     token,
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.UpdateSubscriptionToken(ctx, params)
}

func (s *Service) LinkSubscriptionToUser(ctx context.Context, id, projectID, userID string) (dbwrite.Subscription, error) {
	params := dbwrite.LinkSubscriptionToUserParams{
		UserID:    postgres.StringToText(userID),
		ID:        id,
		ProjectID: projectID,
	}
	return s.repo.LinkSubscriptionToUser(ctx, params)
}

func (s *Service) GetSubscriptionsByProject(ctx context.Context, projectID string) ([]dbread.Subscription, error) {
	return s.repo.GetSubscriptionsByProject(ctx, projectID)
}
