package workers

import (
	"context"

	"github.com/fivebitsio/cotton/internal/deps/logger"
	"github.com/fivebitsio/cotton/internal/workers/subscriptions"
)

func RunSubscription(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return startSubscription(ctx, d)
}

func startSubscription(ctx context.Context, d *deps) error {
	logger.Log.Info("Starting subscription worker...")

	return subscriptions.StartWorker(ctx, d.pgRo, d.pgW, d.nats)
}
