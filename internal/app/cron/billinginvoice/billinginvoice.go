// Package billinginvoice runs one invoicing pass: close every period that is
// due, charge every invoice that is, settle what the charge response could not,
// pin each mandate's next billing date, and report what a person has to fix.
// Meant to run hourly -- anniversaries are spread across the month and retries
// are dated to the hour -- but the cadence is the CronJob's schedule, not a
// constant here.
package billinginvoice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/app/payments"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// passTimeout bounds one pass end to end: the lock is held throughout, so a hang
// would turn every later run into a green no-op.
const passTimeout = 30 * time.Minute

type config struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
}

const (
	outcomeInvoiced    = "invoiced"
	outcomeDisabled    = "disabled"
	outcomeLockHeld    = "lock_held"
	outcomeSetupFailed = "setup_failed"
	outcomeFailed      = "failed"
)

var (
	passCounter    metric.Int64Counter
	invoiceCounter metric.Int64Counter
)

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/cron/billinginvoice")
	passCounter, _ = meter.Int64Counter("billing.invoice_pass_total",
		metric.WithDescription("Invoicing passes by outcome. setup_failed and failed exit non-zero."))
	invoiceCounter, _ = meter.Int64Counter("billing.invoices_total",
		metric.WithDescription("What the pass did to invoices, by outcome. Not every outcome is a status."))
}

// Run invoices once and returns. The error is the CronJob's exit code: a failed
// pass must not come back nil, and lock contention must.
func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	ctx, span := otel.Tracer("cron/billinginvoice").Start(ctx, "billing.invoice")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	outcome := outcomeSetupFailed
	defer func() {
		passCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}()

	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return setupFailed(ctx, "billing config", err)
	}
	if !billingCfg.Enabled {
		outcome = outcomeDisabled
		slog.InfoContext(ctx, "billing is disabled; nothing to invoice")
		return nil
	}
	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "payments config", err)
	}
	// A named provider with no API key is a misconfigured CronJob; no provider at
	// all is the self-hosted shape, where nothing can be charged.
	pay, err := payments.New(ctx, cfg.Provider)
	if err != nil {
		return setupFailed(ctx, "payments provider", err)
	}
	if pay == nil {
		outcome = outcomeDisabled
		slog.InfoContext(ctx, "no payments provider configured; nothing to invoice")
		return nil
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

	svc, err := corebilling.NewService(pgRO, pgW, billingCfg, pay)
	if err != nil {
		return setupFailed(ctx, "billing service", err)
	}

	slog.InfoContext(ctx, "Running a billing invoice pass")
	outcome = outcomeFailed
	err = cron.WithLock(ctx, pgW, cron.JobBillingInvoice, func(ctx context.Context) error {
		return pass(ctx, svc, time.Now())
	})
	if err != nil {
		if errors.Is(err, cron.ErrLockHeld) {
			outcome = outcomeLockHeld
			slog.InfoContext(ctx, "another pass holds the billing invoice lock; nothing to do")
			return nil
		}
		slog.ErrorContext(ctx, "billing invoice pass failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	outcome = outcomeInvoiced
	return nil
}

// pass fails when a provider call failed or a charge is still unresolved: a
// declined card, a cancelled mandate or a period the meter has not reached is a
// finding, not an exit code. Matches the reconcile pass -- one org charging
// successfully says nothing about the ones that did not.
func pass(ctx context.Context, svc *corebilling.Service, now time.Time) error {
	report, err := svc.InvoicePass(ctx, now)
	// Emitted even on a failed pass: the work already done is what says where it
	// stopped.
	for status, n := range map[string]int{
		"open": report.Closed, "waived": report.Waived, "charged": report.Charged,
		"failed": report.Declined, "uncollectible": report.Uncollectible,
		"mandate_gone": report.MandateGone, "dropped": report.Dropped,
		"settled": report.Settled, "reopened": report.Reopened,
		"ambiguous": report.Ambiguous, "held": report.Held,
		"unpriceable": report.Unpriceable, "unbilled": report.Unbilled,
		"foreign": report.Foreign, "unreadable": report.Unreadable,
	} {
		if n > 0 {
			invoiceCounter.Add(ctx, int64(n), metric.WithAttributes(attribute.String("status", status)))
		}
	}
	if err != nil {
		return err
	}
	return exitErr(report)
}

// maxWriteOffsPerPass bounds how many mandates one pass may declare gone. At any
// real scale a burst is pug's own fault -- a flipped environment, a rotated key --
// and writing every open invoice off is not a per-customer finding.
const maxWriteOffsPerPass = 10

// exitErr is the CronJob's whole success signal, kept separate so it can be
// tested without a provider.
func exitErr(report corebilling.InvoiceReport) error {
	if report.Unreadable > 0 {
		return fmt.Errorf("billing invoice pass could not reach the provider: %d calls failed", report.Unreadable)
	}
	if report.Ambiguous > 0 {
		return fmt.Errorf("billing invoice pass left %d charges unresolved", report.Ambiguous)
	}
	if report.MandateGone > maxWriteOffsPerPass {
		return fmt.Errorf("billing invoice pass wrote off %d mandates as gone", report.MandateGone)
	}
	return nil
}

func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("billing invoice setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "billing invoice setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
