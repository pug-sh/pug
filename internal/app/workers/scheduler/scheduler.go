package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
)

const pollInterval = 30 * time.Second

type Scheduler struct {
	read     *dbread.Queries
	producer jetstream.JetStream
}

func Run(ctx context.Context) error {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return err
	}

	pgRO, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	natsClient, err := nats.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	return StartWorker(ctx, pgRO, natsClient)
}

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, natsClient *nats.NATSClient) error {
	s := &Scheduler{
		read:     dbread.New(pgRO),
		producer: natsClient.GetJetStream(),
	}

	slog.InfoContext(ctx, "Starting campaign scheduler", slog.Duration("poll_interval", pollInterval))

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run once immediately on startup.
	s.pollAndPublish(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.pollAndPublish(ctx)
		}
	}
}

func (s *Scheduler) pollAndPublish(ctx context.Context) {
	dueCampaigns, err := s.read.GetScheduledCampaigns(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to poll scheduled campaigns", slogx.Error(err))
		return
	}

	if len(dueCampaigns) == 0 {
		return
	}

	slog.InfoContext(ctx, "Found due campaigns", slog.Int("count", len(dueCampaigns)))

	var failCount int
	for _, c := range dueCampaigns {
		msg := campaigns.CampaignMessage{
			CampaignID: c.ID,
			ProjectID:  c.ProjectID,
		}

		data, err := json.Marshal(msg)
		if err != nil {
			failCount++
			slog.ErrorContext(ctx, "failed to marshal campaign message",
				slogx.Error(err), slog.String("campaign_id", c.ID))
			continue
		}

		if _, err := s.producer.Publish(ctx, nats.CampaignScheduledSubject, data); err != nil {
			failCount++
			slog.ErrorContext(ctx, "failed to publish scheduled campaign",
				slogx.Error(err), slog.String("campaign_id", c.ID))
			continue
		}

		slog.InfoContext(ctx, "Published scheduled campaign",
			slog.String("campaign_id", c.ID),
			slog.String("project_id", c.ProjectID))
	}

	if failCount > 0 {
		slog.WarnContext(ctx, "some scheduled campaigns failed to publish",
			slog.Int("failed", failCount),
			slog.Int("total", len(dueCampaigns)))
	}
}

func init() {
	// Ensure pollInterval is a reasonable value.
	if pollInterval < 10*time.Second {
		panic(fmt.Sprintf("scheduler poll interval too short: %s", pollInterval))
	}
}
