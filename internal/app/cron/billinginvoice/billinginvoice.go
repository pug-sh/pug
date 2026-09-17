// Package billinginvoice runs one invoicing pass: close every period that is due,
// charge what is due, settle what a charge's answer could not, pin each mandate's
// next billing date, and report the usage nothing bills.
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
	coreusage "github.com/pug-sh/pug/internal/core/usage"
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

// maxMandatesGone is how many invoices one pass writes off as their mandate gone
// before the burst is taken for pug's own fault, not its customers'.
const maxMandatesGone = 10

type config struct {
	Provider   string `env:"PUG_BILLING_PROVIDER"`
	RescanDays int    `env:"PUG_USAGE_RESCAN_DAYS"`
}

func (c config) grace() time.Duration {
	return time.Duration(coreusage.RescanDays(c.RescanDays)) * 24 * time.Hour
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
		metric.WithDescription("What each pass did and found, by status. A finding still waiting is counted again on every pass."))
}

// Run invoices once and returns. The error is the CronJob's exit code: a failed
// pass must not come back nil, and lock contention must.
func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	// Before config and pools: RecordError resolves to the noop span until a root
	// span exists, and setup is where a misconfigured deployment fails.
	ctx, span := otel.Tracer("cron/billinginvoice").Start(ctx, "billing.invoice_pass")
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
	// Its CronJob is scheduled before the flag flips.
	if !billingCfg.Enabled {
		outcome = outcomeDisabled
		slog.InfoContext(ctx, "billing is disabled; nothing to invoice")
		return nil
	}
	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "invoice config", err)
	}
	// No provider still closes periods; a charge falling due then fails the pass.
	pay, err := payments.New(ctx, cfg.Provider)
	if err != nil {
		return setupFailed(ctx, "payments provider", err)
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

	svc, err := corebilling.NewService(pgRO, pgW, true, pay)
	if err != nil {
		return setupFailed(ctx, "billing service", err)
	}

	// Logged: the meter's CronJob reads PUG_USAGE_RESCAN_DAYS from its own env.
	grace := cfg.grace()
	slog.InfoContext(ctx, "Running a billing invoice pass", slog.Int("grace_days", int(grace/(24*time.Hour))))
	outcome = outcomeFailed
	err = cron.WithLock(ctx, pgW, cron.JobBillingInvoice, func(ctx context.Context) error {
		return pass(ctx, svc, time.Now(), grace)
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

type report struct {
	close    corebilling.CloseReport
	charge   corebilling.ChargeReport
	settle   corebilling.SettleReport
	pin      corebilling.PinReport
	unbilled int
}

// pass runs every step past an earlier one's failure: each acts only on rows already
// committed. The counters go out either way, since the work done says where it stopped.
func pass(ctx context.Context, svc *corebilling.Service, now time.Time, grace time.Duration) error {
	var (
		r                                                   report
		closeErr, chargeErr, settleErr, pinErr, unbilledErr error
	)
	r.close, closeErr = svc.ClosePeriods(ctx, now, grace)
	r.charge, chargeErr = svc.ChargeDue(ctx, now)
	r.settle, settleErr = svc.SettleCharges(ctx, now)
	r.pin, pinErr = svc.PinNextCharges(ctx, now, grace)
	r.unbilled, unbilledErr = svc.UnbilledUsage(ctx, now, grace)

	var attrs []any
	for _, c := range r.counts() {
		if c.n == 0 {
			continue
		}
		invoiceCounter.Add(ctx, int64(c.n), metric.WithAttributes(attribute.String("status", c.status)))
		attrs = append(attrs, slog.Int(c.status, c.n))
	}
	slog.InfoContext(ctx, "billing invoice pass finished", attrs...)
	return errors.Join(closeErr, chargeErr, settleErr, pinErr, unbilledErr, r.failure())
}

type count struct {
	status string
	n      int
}

func (r report) counts() []count {
	return []count{
		{"closed", r.close.Closed},
		{"waived", r.close.Waived},
		{"deferred", r.close.Deferred},
		{"swept", r.close.Swept},
		{"held", r.close.Held},
		{"unpriceable", r.close.Unpriceable},
		{"dropped", r.close.Dropped},
		{"awaiting_card", r.close.AwaitingCard + r.charge.AwaitingCard},
		{"charged", r.charge.Charged + r.settle.Adopted},
		{"failed", r.charge.Declined + r.settle.Failed},
		{"uncollectible", r.charge.Uncollectible + r.settle.Uncollectible},
		{"mandate_gone", r.charge.MandateGone},
		{"mandate_paused", r.charge.MandatePaused},
		{"ambiguous", r.charge.Ambiguous},
		{"paid", r.settle.Paid},
		{"reopened", r.settle.Reopened},
		{"pending", r.settle.Pending},
		{"duplicate", r.settle.Duplicate},
		{"amount_mismatch", r.settle.AmountMismatch},
		{"pinned", r.pin.Pinned},
		{"unreadable", r.charge.Unreadable + r.settle.Unreadable + r.pin.Unreadable},
		{"unbilled", r.unbilled},
	}
}

// failure turns the CronJob red only when the pass itself is broken: a declined card
// or a stale meter is a finding for a person, not an exit code.
func (r report) failure() error {
	var errs []error
	if n := r.charge.Unreadable + r.settle.Unreadable + r.pin.Unreadable; n > 0 {
		errs = append(errs, fmt.Errorf("billing invoice pass could not read or record the provider %d times", n))
	}
	if r.charge.Ambiguous > 0 {
		errs = append(errs, fmt.Errorf("billing invoice pass left %d charges unresolved", r.charge.Ambiguous))
	}
	if r.close.Dropped > 0 {
		errs = append(errs, fmt.Errorf("billing invoice pass wrote off %d periods no pass had billed", r.close.Dropped))
	}
	if r.charge.MandateGone > maxMandatesGone {
		errs = append(errs, fmt.Errorf("billing invoice pass wrote off %d invoices as their mandate gone", r.charge.MandateGone))
	}
	return errors.Join(errs...)
}

// setupFailed reports a dependency that would not come up. Wrapped so main's
// stderr line, printed after telemetry shut down, still names the step.
func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("billing invoice setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "billing invoice setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
