package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/delivery"
	devicessvc "github.com/fivebitsio/cotton/internal/core/devices"
	"github.com/fivebitsio/cotton/internal/core/projects"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/slogx"
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
		return natsworker.NewPermanentError(fmt.Errorf("failed to unmarshal campaign message: %w", err))
	}

	if msg.CampaignID == "" {
		return natsworker.NewPermanentError(fmt.Errorf("campaign message missing campaign_id"))
	}

	slog.InfoContext(ctx, "Processing campaign", slog.String("campaign_id", msg.CampaignID))

	campaign, err := w.campaignService.GetCampaignByID(ctx, msg.CampaignID)
	if err != nil {
		return fmt.Errorf("failed to get campaign %s: %w", msg.CampaignID, err)
	}

	if err := w.campaignService.UpdateCampaignStatus(ctx, campaign.ID, campaigns.StatusInProgress); err != nil {
		slog.ErrorContext(ctx, "failed to set campaign in-progress", slogx.Error(err), slog.String("campaign_id", campaign.ID))
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
			return fmt.Errorf("failed to get active devices for project %s: %w", campaign.ProjectID, err)
		}

		for _, device := range devices {
			if err := w.deliveryService.SendNotification(ctx, campaign, device); err != nil {
				failCount++
				slog.ErrorContext(ctx, "failed to send notification",
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
		return fmt.Errorf("campaign %s: all %d deliveries failed", campaign.ID, totalCount)
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
		return fmt.Errorf("failed to update campaign %s status to %s: %w", campaign.ID, finalStatus, err)
	}

	return nil
}
