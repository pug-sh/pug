package campaigns

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	pulsarclient "github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	repo        Repo
	projectsSvc *projects.Service
	producer    *pulsarclient.Producer
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service, producer *pulsarclient.Producer) *Service {
	return &Service{
		repo:        NewRepo(pgRO, pgW),
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

	if err := s.sendCampaignToPulsar(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send campaign to pulsar", slog.Any("error", err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
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
	campaign, err := s.repo.UpdateCampaign(ctx, arg)
	if err != nil {
		return campaign, err
	}

	scheduledTime := arg.ScheduledTime.Time

	if err := s.sendCampaignToPulsar(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send updated campaign to pulsar", slog.Any("error", err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
}

func (s *Service) sendCampaignToPulsar(ctx context.Context, campaign dbwrite.Campaign, scheduledTime time.Time) error {
	deliverAt := &scheduledTime

	pulsarMsg := &pulsarclient.Message{
		Payload: campaign.NotificationData,
		Properties: map[string]string{
			"campaign_id": campaign.ID,
			"project_id":  campaign.ProjectID,
		},
		DeliverAt: deliverAt,
	}

	if err := s.producer.Send(ctx, pulsarMsg); err != nil {
		return fmt.Errorf("failed to send campaign to pulsar: %w", err)
	}

	return nil
}

func (s *Service) ProjectExistsForCustomer(ctx context.Context, projectID string, customerID string) (bool, error) {
	return s.projectsSvc.ProjectExistsForCustomer(ctx, projectID, customerID)
}
