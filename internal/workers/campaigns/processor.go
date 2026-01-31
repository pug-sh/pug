package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/delivery"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/core/subscriptions"
	"github.com/fivebitsio/cotton/pkg/logger/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	campaignService     *campaigns.Service
	subscriptionService *subscriptions.Service
	deliveryService     delivery.Service
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	projectsSvc := projects.NewService(pgRO, pgW)
	return &Worker{
		campaignService:     campaigns.NewService(pgRO, pgW, projectsSvc, nil),
		subscriptionService: subscriptions.NewService(pgRO, pgW),
		deliveryService:     delivery.NewRouter(pgRO, pgW, projectsSvc),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	var msg campaigns.CampaignMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("failed to unmarshal campaign message: %w", err)
	}

	if msg.CampaignID == "" {
		return fmt.Errorf("campaign message missing campaign_id")
	}

	slog.Info("Processing campaign", slog.String("campaign_id", msg.CampaignID))

	campaign, err := w.campaignService.GetCampaignByID(ctx, msg.CampaignID)
	if err != nil {
		return fmt.Errorf("failed to get campaign %s: %w", msg.CampaignID, err)
	}

	subscriptions, err := w.subscriptionService.GetSubscriptionsByProject(ctx, campaign.ProjectID)
	if err != nil {
		return fmt.Errorf("failed to get subscriptions for project %s: %w", campaign.ProjectID, err)
	}

	slog.Info("Processing subscriptions for campaign",
		slog.String("campaign_id", campaign.ID),
		slog.String("project_id", campaign.ProjectID),
		slog.Int("subscription_count", len(subscriptions)))

	var failCount int
	for _, sub := range subscriptions {
		if string(sub.Status) == "active" {
			if err := w.deliveryService.SendNotification(ctx, campaign, sub); err != nil {
				failCount++
				slog.ErrorContext(ctx, "failed to send notification",
					slog.String("subscription_id", sub.ID),
					slog.String("campaign_id", campaign.ID),
					slogx.Error(err))
			}
		}
	}

	if failCount > 0 {
		return fmt.Errorf("failed to deliver %d notifications for campaign %s", failCount, campaign.ID)
	}

	return nil
}
