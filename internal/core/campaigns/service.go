package campaigns

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/nats"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
)

type Service struct {
	repo        *repo
	projectsSvc *projects.Service
	producer    jetstream.JetStream
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service, producer jetstream.JetStream) *Service {
	return &Service{
		repo:        newRepo(pgRO, pgW),
		projectsSvc: projectsSvc,
		producer:    producer,
	}
}

func (s *Service) CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	campaign, err := s.repo.CreateCampaign(ctx, arg)
	if err != nil {
		return campaign, err
	}

	scheduledTime := arg.ScheduledTime.Time

	if err := s.sendCampaignToNATS(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send campaign to NATS", slog.Any("error", err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
}

func (s *Service) GetCampaignById(ctx context.Context, id string) (dbread.Campaign, error) {
	return s.repo.GetCampaignById(ctx, id)
}

func (s *Service) GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error) {
	return s.repo.GetCampaignsByProjectID(ctx, projectID)
}

func (s *Service) GetScheduledCampaigns(ctx context.Context) ([]dbread.Campaign, error) {
	return s.repo.GetScheduledCampaigns(ctx)
}

func (s *Service) DeleteCampaign(ctx context.Context, id string, projectID string) error {
	return s.repo.DeleteCampaign(ctx, dbwrite.DeleteCampaignParams{
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	campaign, err := s.repo.UpdateCampaign(ctx, arg)
	if err != nil {
		return campaign, err
	}

	scheduledTime := arg.ScheduledTime.Time

	if err := s.sendCampaignToNATS(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send updated campaign to NATS", slog.Any("error", err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
}

func (s *Service) sendCampaignToNATS(ctx context.Context, campaign dbwrite.Campaign, scheduledTime time.Time) error {
	// For NATS, we'll publish immediately since NATS JetStream doesn't have a direct equivalent to Pulsar's DeliverAt
	// Instead, we could implement a delayed delivery mechanism using NATS timers if needed

	_, err := s.producer.Publish(ctx, nats.CampaignScheduledSubject, campaign.NotificationData)
	if err != nil {
		return fmt.Errorf("failed to send campaign to NATS: %w", err)
	}

	return nil
}

func (s *Service) ProjectExistsForCustomer(ctx context.Context, projectID string, customerID string) (bool, error) {
	return s.projectsSvc.ProjectExistsForCustomer(ctx, projectID, customerID)
}
