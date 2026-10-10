// Package retention runs one retention pass: it moves every org's retention state
// on, deletes the event history past each org's cut, then returns. Scheduling is
// the deployment's job (a daily k8s CronJob).
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coreretention "github.com/pug-sh/pug/internal/core/retention"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
)

// passTimeout bounds one pass. The lock is held throughout, so a hang would turn
// every later run into a green no-op. The deletes are queued, not waited on.
const passTimeout = 30 * time.Minute

// Run passes once and returns. The error is the CronJob's exit code; lock
// contention is not a failure.
func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	// Before config and pools: RecordError resolves to the noop span until a root
	// span exists, and setup is where a misconfigured deployment fails.
	ctx, span := otel.Tracer("cron/retention").Start(ctx, "retention.pass")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	var cfg coreretention.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "retention config", err)
	}
	// The server's switch and default length, so the two resolve every org alike.
	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return setupFailed(ctx, "billing config", err)
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return setupFailed(ctx, "postgres config", err)
	}
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres writer pool", err)
	}
	defer pgW.Close()

	var chCfg chdb.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return setupFailed(ctx, "clickhouse config", err)
	}
	ch, err := chdb.NewWriterPool(ctx, &chCfg)
	if err != nil {
		return setupFailed(ctx, "clickhouse writer pool", err)
	}
	defer func() {
		if err := ch.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()

	ents, err := entitlement.NewService(pgW, pgW, billingCfg)
	if err != nil {
		return setupFailed(ctx, "entitlement service", err)
	}
	svc := coreretention.NewService(pgW, ch, ents, cfg)
	err = cron.WithLock(ctx, pgW, cron.JobRetention, func(ctx context.Context) error {
		return svc.Pass(ctx, time.Now().UTC())
	})
	if err != nil {
		if errors.Is(err, cron.ErrLockHeld) {
			slog.InfoContext(ctx, "another pass holds the retention lock; nothing to do")
			return nil
		}
		// Each step recorded its own error; this marks the pass's root span.
		slog.ErrorContext(ctx, "retention pass failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	return nil
}

// setupFailed reports a dependency that would not come up. Wrapped so main's
// stderr line, printed after telemetry shut down, still names the step.
func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("retention setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "retention setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
