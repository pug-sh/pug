// Package meter runs one billing meter pass and exits: every live subscription's
// per-tier usage is stated to the provider's meters. Deployed as an hourly CronJob
// (cmd/cron/billing-meter); the cadence is the CronJob's schedule, not a constant.
package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/app/payments"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coremeter "github.com/pug-sh/pug/internal/core/billing/meter"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
)

// passTimeout bounds one pass end to end, like the reconcile's: the advisory lock is
// held throughout, so a hang would turn every later tick into a green no-op. It also
// bounds every statement's timestamp — the tick's start — which the provider
// rejects once it is an hour old.
const passTimeout = 30 * time.Minute

type config struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
}

// Run is one pass. Billing off exits 0 having built nothing; billing on without a
// provider that meters, or with a product that would bill a tier wrongly, is a
// setup failure the CronJob reports.
func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	// Before config and pools: RecordError resolves to the noop span until a root
	// span exists, and setup is where a misconfigured deployment fails.
	ctx, span := otel.Tracer("cron/meter").Start(ctx, "billing.meter")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return setupFailed(ctx, "billing config", err)
	}
	if !billingCfg.Enabled {
		slog.InfoContext(ctx, "billing is disabled; nothing to meter")
		return nil
	}
	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "payments config", err)
	}
	pay, err := payments.New(ctx, cfg.Provider)
	if err != nil {
		return setupFailed(ctx, "payments provider", err)
	}
	// Unlike the reconcile, a pass with no provider has no other work to do: a
	// billed deployment that meters nothing bills every subscriber its fee alone.
	if !pay.Configured() || pay.Usage == nil {
		return setupFailed(ctx, "payments provider", errors.New("billing is on but no provider that meters usage is configured"))
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return setupFailed(ctx, "postgres config", err)
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres reader pool", err)
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres writer pool", err)
	}
	defer pgW.Close()

	entitlements, err := entitlement.NewService(pgRO, pgW, true)
	if err != nil {
		return setupFailed(ctx, "entitlement service", err)
	}

	// Before stating anything: a product missing a meter bills that tier at zero,
	// and one with a free threshold applies the allowance twice, both silently.
	for slug, productID := range pay.ProductBySlug {
		plan, ok := entitlement.PlanBySlug(slug)
		if !ok {
			continue
		}
		if err := pay.Usage.VerifyMetering(ctx, plan.Tiers(), []string{productID}); err != nil {
			return setupFailed(ctx, "metering configuration", err)
		}
	}

	svc := coremeter.NewService(pgRO, pgW, entitlements, pay.Usage, pay.Provider.Name())
	slog.InfoContext(ctx, "Running a billing meter pass")
	err = cron.WithLock(ctx, pgW, cron.JobBillingMeter, func(ctx context.Context) error {
		return pass(ctx, svc, time.Now())
	})
	if err != nil {
		// Another pod is doing this work: exit 0, but say so, or "skipped" and
		// "metered" are the same silent success.
		if errors.Is(err, cron.ErrLockHeld) {
			slog.InfoContext(ctx, "another pass holds the billing meter lock; nothing to do")
			return nil
		}
		slog.ErrorContext(ctx, "billing meter pass failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	return nil
}

// pass is Run's work, apart so a test can drive it.
func pass(ctx context.Context, svc *coremeter.Service, now time.Time) error {
	report, err := svc.Run(ctx, now)
	slog.InfoContext(ctx, "billing meter pass",
		slog.Int("orgs", report.Orgs), slog.Int("stated", report.Stated), slog.Int("unchanged", report.Unchanged),
		slog.Int("frozen", report.Frozen), slog.Int("resent", report.Resent), slog.Int("failed", report.Failed))
	return err
}

// setupFailed reports a dependency that would not come up. Wrapped so main's
// stderr line, printed after telemetry shut down, still names the step.
func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("billing meter setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "billing meter setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
