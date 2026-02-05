package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	StatusScheduled  = "scheduled"
	StatusInProgress = "in-progress"
	StatusComplete   = "complete"
	StatusFail       = "fail"
)

type Service struct {
	read        *dbread.Queries
	write       *dbwrite.Queries
	projectsSvc *projects.Service
	producer    jetstream.JetStream
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, projectsSvc *projects.Service, producer jetstream.JetStream) *Service {
	return &Service{
		read:        dbread.New(pgRO),
		write:       dbwrite.New(pgW),
		projectsSvc: projectsSvc,
		producer:    producer,
	}
}

func (s *Service) CreateCampaign(ctx context.Context, arg dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	campaign, err := s.write.CreateCampaign(ctx, arg)
	if err != nil {
		return campaign, err
	}

	scheduledTime := arg.ScheduledTime.Time

	if err := s.sendCampaignToNATS(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send campaign to NATS", slogx.Error(err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
}

func (s *Service) GetCampaignByID(ctx context.Context, id string) (dbread.Campaign, error) {
	return s.read.GetCampaignByID(ctx, id)
}

func (s *Service) GetCampaignByIDAndProjectID(ctx context.Context, id string, projectID string) (dbread.Campaign, error) {
	return s.read.GetCampaignByIDAndProjectID(ctx, dbread.GetCampaignByIDAndProjectIDParams{
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) GetCampaignsByProjectID(ctx context.Context, projectID string) ([]dbread.Campaign, error) {
	return s.read.GetCampaignsByProjectID(ctx, projectID)
}

func (s *Service) GetScheduledCampaigns(ctx context.Context) ([]dbread.Campaign, error) {
	return s.read.GetScheduledCampaigns(ctx)
}

func (s *Service) DeleteCampaign(ctx context.Context, id string, projectID string) error {
	return s.write.DeleteCampaign(ctx, dbwrite.DeleteCampaignParams{
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) UpdateCampaign(ctx context.Context, arg dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	campaign, err := s.write.UpdateCampaign(ctx, arg)
	if err != nil {
		return campaign, err
	}

	scheduledTime := arg.ScheduledTime.Time

	if err := s.sendCampaignToNATS(ctx, campaign, scheduledTime); err != nil {
		slog.ErrorContext(ctx, "failed to send updated campaign to NATS", slogx.Error(err), slog.String("campaignId", campaign.ID))
	}

	return campaign, nil
}

// CampaignMessage is the payload published to NATS for campaign processing.
type CampaignMessage struct {
	CampaignID string `json:"campaign_id"`
	ProjectID  string `json:"project_id"`
}

func (s *Service) sendCampaignToNATS(ctx context.Context, campaign dbwrite.Campaign, scheduledTime time.Time) error {
	if scheduledTime.After(time.Now()) {
		slog.InfoContext(ctx, "campaign scheduled for the future, skipping immediate publish",
			slog.String("campaign_id", campaign.ID),
			slog.Time("scheduled_time", scheduledTime))
		return nil
	}

	msg := CampaignMessage{
		CampaignID: campaign.ID,
		ProjectID:  campaign.ProjectID,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal campaign message: %w", err)
	}

	_, err = s.producer.Publish(ctx, nats.CampaignScheduledSubject, data)
	if err != nil {
		return fmt.Errorf("failed to send campaign to NATS: %w", err)
	}

	return nil
}

func (s *Service) UpdateCampaignStatus(ctx context.Context, id, status string) error {
	_, err := s.write.UpdateCampaignStatus(ctx, dbwrite.UpdateCampaignStatusParams{
		ID:     id,
		Status: status,
	})
	return err
}

func (s *Service) ProjectExistsForCustomer(ctx context.Context, projectID string, customerID string) (bool, error) {
	return s.projectsSvc.ProjectExistsForCustomer(ctx, projectID, customerID)
}
