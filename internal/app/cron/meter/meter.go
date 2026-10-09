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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/app/payments"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coremeter "github.com/pug-sh/pug/internal/core/billing/meter"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// passTimeout bounds one pass end to end, like the reconcile's: the advisory lock is
// held throughout, so a hang would turn every later tick into a green no-op. It also
// bounds every statement's timestamp — the tick's start — which the provider
// rejects once it is an hour old.
const passTimeout = 30 * time.Minute

const (
	outcomeMetered     = "metered"
	outcomeIdle        = "idle"
	outcomeLockHeld    = "lock_held"
	outcomeStaleUsage  = "stale_usage"
	outcomeSetupFailed = "setup_failed"
	outcomeFailed      = "failed"
)

var (
	passCounter  metric.Int64Counter
	passDuration metric.Float64Histogram
	orgCounter   metric.Int64Counter
)

// Machine-readable companions to the pass's log lines, as the usage pass has.
// Registration errors are dropped rather than fatal: the exit code is the CronJob's
// alert, and these only explain it.
func init() {
	m := otel.Meter("github.com/pug-sh/pug/internal/app/cron/meter")
	passCounter, _ = m.Int64Counter(
		"billing_meter.pass_total",
		metric.WithDescription("Billing meter passes by outcome. metered, idle (billing off, or nothing to meter) and lock_held exit 0; stale_usage, setup_failed and failed exit non-zero. A telemetry setup failure emits no sample at all."),
	)
	passDuration, _ = m.Float64Histogram(
		"billing_meter.pass_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Wall time of one pass, tagged by outcome. Approaching passTimeout means the pass is about to be cut short and later ones to find the lock held."),
	)
	orgCounter, _ = m.Int64Counter(
		"billing_meter.orgs_total",
		metric.WithDescription("Live subscriptions by what the pass did with them: stated, unchanged, frozen (waiting on a renewal), superseded (changed mid-pass) or failed."),
	)
}

type config struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
}

// Run is one pass. A deployment that cannot meter is healthy exactly when nobody is
// paying: billing off exits 0, warning about any live subscription left behind, and
// billing on without a provider that meters exits 0 with no live subscription and
// fails with one. A product that would bill a tier wrongly is a setup failure.
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

	// Registered after cancel so LIFO runs it first, on a context still live.
	start := time.Now()
	outcome := outcomeSetupFailed
	defer func() {
		attrs := metric.WithAttributes(attribute.String("outcome", outcome))
		passCounter.Add(ctx, 1, attrs)
		passDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	}()

	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return setupFailed(ctx, "billing config", err)
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

	// Whether a pass that cannot meter is healthy depends on whether anybody is
	// paying, so this is counted before either answer.
	live, err := dbread.New(pgRO).CountLiveBillingSubscriptions(ctx)
	if err != nil {
		return setupFailed(ctx, "live subscriptions", err)
	}
	if !billingCfg.Enabled {
		outcome = outcomeIdle
		if live > 0 {
			// The operator's choice, so not a failure. But the provider keeps charging
			// these subscribers their fee, and nothing states their usage.
			slog.WarnContext(ctx, "billing is disabled while orgs hold live subscriptions; their usage is not stated",
				slog.Int64("live_subscriptions", live))
			return nil
		}
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
	if !pay.Configured() || pay.Usage == nil {
		// Billing on with no provider is a supported mode — allowances with no buy
		// button — while nobody pays. A subscriber whose usage nothing states pays the
		// fee alone.
		if live == 0 {
			outcome = outcomeIdle
			slog.InfoContext(ctx, "no provider that meters usage is configured, and no org holds a live subscription; nothing to meter")
			return nil
		}
		return setupFailed(ctx, "payments provider",
			fmt.Errorf("billing is on and %d orgs hold a live subscription, but no provider that meters usage is configured", live))
	}

	entitlements, err := entitlement.NewService(pgRO, pgW, billingCfg)
	if err != nil {
		return setupFailed(ctx, "entitlement service", err)
	}
	if err := verifyCatalog(ctx, pay); err != nil {
		return setupFailed(ctx, "metering configuration", err)
	}

	svc := coremeter.NewService(pgRO, pgW, entitlements, pay.Usage, pay.Provider.Name())
	slog.InfoContext(ctx, "Running a billing meter pass")
	outcome, err = meterUnderLock(ctx, pgW, svc, time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "billing meter pass failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	return nil
}

// verifyCatalog checks every catalog product before anything is stated: a product
// missing a meter bills that tier at zero, one with a free threshold applies the
// allowance twice, and one with any other meter attached bills what pug never
// states — all silently. A deal's product is reconcile's to check.
func verifyCatalog(ctx context.Context, pay *corebilling.Payments) error {
	for slug, productID := range pay.ProductBySlug {
		plan, ok := entitlement.PlanBySlug(slug)
		if !ok {
			continue
		}
		if err := pay.Usage.VerifyMetering(ctx, plan.Tiers(), []string{productID}); err != nil {
			return err
		}
	}
	return nil
}

// meterUnderLock runs the pass holding the job's lock and names its outcome.
// Another pod holding the lock is doing this work: exit 0, but say so, or
// "skipped" and "metered" are the same silent success.
func meterUnderLock(ctx context.Context, pgW *pgxpool.Pool, svc *coremeter.Service, now time.Time) (string, error) {
	var report coremeter.Report
	err := cron.WithLock(ctx, pgW, cron.JobBillingMeter, func(ctx context.Context) error {
		var err error
		report, err = pass(ctx, svc, now)
		return err
	})
	switch {
	case errors.Is(err, cron.ErrLockHeld):
		slog.InfoContext(ctx, "another pass holds the billing meter lock; nothing to do")
		return outcomeLockHeld, nil
	case errors.Is(err, coremeter.ErrUsageStale):
		return outcomeStaleUsage, err
	case err != nil:
		return outcomeFailed, err
	case report.Orgs == 0:
		return outcomeIdle, nil
	}
	return outcomeMetered, nil
}

// pass is the locked work, logged and counted whatever it returns.
func pass(ctx context.Context, svc *coremeter.Service, now time.Time) (coremeter.Report, error) {
	report, err := svc.Run(ctx, now)
	slog.InfoContext(ctx, "billing meter pass",
		slog.Int("orgs", report.Orgs), slog.Int("stated", report.Stated), slog.Int("unchanged", report.Unchanged),
		slog.Int("frozen", report.Frozen), slog.Int("superseded", report.Superseded), slog.Int("resent", report.Resent),
		slog.Int("failed", report.Failed))
	for outcome, n := range map[string]int{
		"stated": report.Stated, "unchanged": report.Unchanged, "frozen": report.Frozen,
		"superseded": report.Superseded, "failed": report.Failed,
	} {
		orgCounter.Add(ctx, int64(n), metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	return report, err
}

// setupFailed reports a dependency that would not come up. Wrapped so main's
// stderr line, printed after telemetry shut down, still names the step.
func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("billing meter setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "billing meter setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
