package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/pug-sh/pug/internal/core/campaigns"
	"github.com/pug-sh/pug/internal/core/delivery"
	devicessvc "github.com/pug-sh/pug/internal/core/devices"
	"github.com/pug-sh/pug/internal/core/projects"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	campaignService *campaigns.Service
	deviceService   *devicessvc.Service
	deliveryService delivery.Service
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	projectsSvc := projects.NewService(pgRO, pgW, nil)
	return &Worker{
		campaignService: campaigns.NewService(pgRO, pgW, projectsSvc, nil),
		deviceService:   devicessvc.NewService(pgRO, pgW),
		deliveryService: delivery.NewRouter(pgRO, pgW, projectsSvc),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	var msg campaigns.CampaignMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal campaign message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(fmt.Errorf("failed to unmarshal campaign message: %w", err)).
			With("worker", "campaigns")
	}

	if msg.CampaignID == "" {
		err := fmt.Errorf("campaign message missing campaign_id")
		slog.ErrorContext(ctx, "campaign message missing campaign_id", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "campaigns")
	}

	slog.InfoContext(ctx, "Processing campaign", slog.String("campaign_id", msg.CampaignID))

	campaign, err := w.campaignService.GetCampaignByID(ctx, msg.CampaignID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get campaign", slogx.Error(err), slog.String("campaign_id", msg.CampaignID))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("failed to get campaign %s: %w", msg.CampaignID, err)
	}

	if err := w.campaignService.UpdateCampaignStatus(ctx, campaign.ID, campaigns.StatusInProgress); err != nil {
		slog.ErrorContext(ctx, "failed to set campaign in-progress", slogx.Error(err), slog.String("campaign_id", campaign.ID))
		telemetry.RecordError(ctx, err)
		return err
	}

	const pageSize = 100
	var (
		afterID    string
		failCount  int
		totalCount int
	)
	for {
		devices, err := w.deviceService.GetActiveDevicesByProject(ctx, campaign.ProjectID, afterID, pageSize)
		if err != nil {
			slog.ErrorContext(ctx, "failed to get active devices", slogx.Error(err), slog.String("project_id", campaign.ProjectID), slog.String("campaign_id", campaign.ID))
			telemetry.RecordError(ctx, err)
			return fmt.Errorf("failed to get active devices for project %s: %w", campaign.ProjectID, err)
		}

		for _, device := range devices {
			if err := w.deliveryService.SendNotification(ctx, campaign, device); err != nil {
				failCount++
				// SendNotification logs+records at source per the log-at-source convention;
				// this Warn captures only the wrapper disposition (one device counted as failed).
				slog.WarnContext(ctx, "device notification counted as failed",
					slog.String("device_id", device.ID),
					slog.String("campaign_id", campaign.ID),
					slogx.Error(err))
			}
		}

		totalCount += len(devices)
		if len(devices) < pageSize {
			break
		}
		afterID = devices[len(devices)-1].ID
	}

	// All deliveries failed — return error to retry via Nak (campaign stays InProgress).
	if failCount > 0 && failCount == totalCount {
		err := fmt.Errorf("campaign %s: all %d deliveries failed", campaign.ID, totalCount)
		slog.ErrorContext(ctx, "all campaign deliveries failed", slogx.Error(err), slog.String("campaign_id", campaign.ID), slog.Int("total_count", totalCount))
		telemetry.RecordError(ctx, err)
		return err
	}

	finalStatus := campaigns.StatusComplete
	if failCount > 0 {
		finalStatus = campaigns.StatusFail
		slog.WarnContext(ctx, "campaign delivered with partial failures",
			slog.String("campaign_id", campaign.ID),
			slog.Int("fail_count", failCount),
			slog.Int("total_count", totalCount))
	}

	if err := w.campaignService.UpdateCampaignStatus(ctx, campaign.ID, finalStatus); err != nil {
		slog.ErrorContext(ctx, "failed to update campaign status", slogx.Error(err), slog.String("campaign_id", campaign.ID), slog.String("status", finalStatus))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("failed to update campaign %s status to %s: %w", campaign.ID, finalStatus, err)
	}

	return nil
}
