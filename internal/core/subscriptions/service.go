package subscriptions

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusActive   = "active"
	StatusInactive = "inactive"
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

func (s *Service) CreateSubscription(ctx context.Context, id, projectID, token, platform string, metadata map[string]any, status, updater string) (dbwrite.Subscription, error) {
	params := dbwrite.CreateSubscriptionParams{
		ID:        id,
		ProjectID: projectID,
		Token:     token,
		Platform:  platform,
		Metadata:  metadata,
		Status:    status,
		Updater:   updater,
	}
	return s.write.CreateSubscription(ctx, params)
}

func (s *Service) GetSubscription(ctx context.Context, id, projectID string) (dbwrite.Subscription, error) {
	params := dbwrite.GetSubscriptionParams{
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.GetSubscription(ctx, params)
}

func (s *Service) UpdateSubscriptionHeartbeat(ctx context.Context, id, projectID string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionHeartbeatParams{
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.UpdateSubscriptionHeartbeat(ctx, params)
}

func (s *Service) UpdateSubscriptionMetadata(ctx context.Context, id, projectID string, metadata map[string]any) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionMetadataParams{
		Metadata:  metadata,
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.UpdateSubscriptionMetadata(ctx, params)
}

func (s *Service) UpdateSubscriptionPlatform(ctx context.Context, id, projectID, platform string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionPlatformParams{
		Platform:  platform,
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.UpdateSubscriptionPlatform(ctx, params)
}

func (s *Service) UpdateSubscriptionStatus(ctx context.Context, id, projectID, status string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionStatusParams{
		Status:    status,
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.UpdateSubscriptionStatus(ctx, params)
}

func (s *Service) UpdateSubscriptionToken(ctx context.Context, id, projectID, token string) (dbwrite.Subscription, error) {
	params := dbwrite.UpdateSubscriptionTokenParams{
		Token:     token,
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.UpdateSubscriptionToken(ctx, params)
}

func (s *Service) LinkSubscriptionToUser(ctx context.Context, id, projectID, userID string) (dbwrite.Subscription, error) {
	params := dbwrite.LinkSubscriptionToUserParams{
		UserID:    postgres.StringToText(userID),
		ID:        id,
		ProjectID: projectID,
	}
	return s.write.LinkSubscriptionToUser(ctx, params)
}

func (s *Service) GetSubscriptionsByProject(ctx context.Context, projectID string) ([]dbread.Subscription, error) {
	return s.read.GetSubscriptionsByProject(ctx, projectID)
}
