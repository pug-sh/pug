package workers

import (
	"context"

	"github.com/fivebitsio/cotton/internal/deps/logger"
	"github.com/fivebitsio/cotton/internal/workers/campaigns"
)

func RunCampaign(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return startCampaign(ctx, d)
}

func startCampaign(ctx context.Context, d *deps) error {
	logger.Log.Info("Starting campaign worker...")

	return campaigns.StartWorker(ctx, d.pgRo, d.pgW, d.nats)
}
