package campaigns

import (
	"context"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/subscriptions"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	campaignService     *campaigns.Service
	subscriptionService *subscriptions.Service
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		campaignService:     campaigns.NewService(pgRO, pgW, nil, nil),
		subscriptionService: subscriptions.NewService(pgRO, pgW),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	slog.Info("Processing campaign message", slog.Int("data_length", len(data)))

	scheduledCampaigns, err := w.campaignService.GetScheduledCampaigns(ctx)
	if err != nil {
		slog.Error("failed to get scheduled campaigns", slog.Any("err", err))
		return err
	}

	slog.Info("Found scheduled campaigns", slog.Int("count", len(scheduledCampaigns)))

	for _, campaign := range scheduledCampaigns {
		subscriptions, err := w.subscriptionService.GetSubscriptionsByProject(ctx, campaign.ProjectID)
		if err != nil {
			slog.Error("failed to get subscriptions for project",
				slog.String("project_id", campaign.ProjectID),
				slog.Any("err", err))
			continue
		}

		slog.Info("Processing subscriptions for campaign",
			slog.String("campaign_id", campaign.ID),
			slog.String("project_id", campaign.ProjectID),
			slog.Int("subscription_count", len(subscriptions)))

		for _, sub := range subscriptions {
			slog.Info("Subscription details",
				slog.String("subscription_id", sub.ID),
				slog.String("platform", sub.Platform),
				slog.String("status", string(sub.Status)))
		}
	}

	return nil
}
