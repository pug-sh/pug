package campaigns

import (
	"context"
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
	slog.Info("Processing campaign message", slog.Int("data_length", len(data)))

	scheduledCampaigns, err := w.campaignService.GetScheduledCampaigns(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get scheduled campaigns", slogx.Error(err))
		return err
	}

	slog.Info("Found scheduled campaigns", slog.Int("count", len(scheduledCampaigns)))

	for _, campaign := range scheduledCampaigns {
		subscriptions, err := w.subscriptionService.GetSubscriptionsByProject(ctx, campaign.ProjectID)
		if err != nil {
			slog.ErrorContext(ctx, "failed to get subscriptions for project",
				slog.String("project_id", campaign.ProjectID), slogx.Error(err))
			continue
		}

		slog.Info("Processing subscriptions for campaign",
			slog.String("campaign_id", campaign.ID),
			slog.String("project_id", campaign.ProjectID),
			slog.Int("subscription_count", len(subscriptions)))

		for _, sub := range subscriptions {
			// Only send notification to active subscriptions
			if string(sub.Status) == "active" {
				if err := w.deliveryService.SendNotification(ctx, campaign, sub); err != nil {
					slog.ErrorContext(ctx, "failed to send notification",
						slog.String("subscription_id", sub.ID),
						slog.String("campaign_id", campaign.ID),
						slogx.Error(err))
				}
			}
		}
	}

	return nil
}
