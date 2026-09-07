// Package billing runs one payments reconcile pass: re-read every stored
// subscription, apply each through the same CAS the webhook uses, report what it
// cannot fix, prune expired payloads, and return. Nothing here auto-repairs.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
)

// passTimeout bounds one pass end to end. The advisory lock is held for the whole
// duration, so a hang leaves every later pod exiting 0 on a held lock -- a green
// CronJob and no reconcile. Generous: one provider round-trip per subscription.
const passTimeout = 30 * time.Minute

type config struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
}

func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	// Before config and pools: RecordError resolves to the noop span until a root
	// span exists, and setup is exactly where a misconfigured deployment fails.
	ctx, span := otel.Tracer("cron/billing").Start(ctx, "billing.reconcile")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return setupFailed(ctx, "billing config", err)
	}
	// Off is the self-hosted shape: nothing to reconcile. Exit 0, so the CronJob stays
	// green on a deployment that simply does not bill.
	if !billingCfg.Enabled {
		slog.InfoContext(ctx, "billing is disabled; nothing to reconcile")
		return nil
	}

	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "payments config", err)
	}
	payments, err := newPayments(ctx, cfg.Provider)
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

	svc, err := corebilling.NewService(pgRO, pgW, billingCfg.Enabled, payments)
	if err != nil {
		return setupFailed(ctx, "billing service", err)
	}

	slog.InfoContext(ctx, "Running a billing reconcile pass")
	err = cron.WithLock(ctx, pgW, cron.JobBillingReconcile, func(ctx context.Context) error {
		return pass(ctx, svc, time.Now())
	})
	if err != nil {
		// Another pod is doing this work. Exit 0 -- but say so, or "skipped" and
		// "reconciled" are the same silent success.
		if errors.Is(err, cron.ErrLockHeld) {
			slog.InfoContext(ctx, "another pass holds the billing reconcile lock; nothing to do")
			return nil
		}
		slog.ErrorContext(ctx, "billing reconcile pass failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	return nil
}

func pass(ctx context.Context, svc *corebilling.Service, now time.Time) error {
	report, err := svc.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	// Ahead of the failure below: a provider outage is no reason to keep processed
	// payloads past their retention, and the prune touches rows reconcile ignores.
	pruned, err := svc.PruneDeliveries(ctx, now.Add(-corebilling.DeliveryRetention))
	if err != nil {
		return err
	}
	if pruned > 0 {
		slog.InfoContext(ctx, "pruned billing webhook deliveries", slog.Int64("rows", pruned))
	}
	// A pass that could not read the provider has verified nothing, and exiting 0
	// would report that as consistent. The other counters are findings, not failures.
	if report.Unreadable > 0 {
		return fmt.Errorf("billing reconcile could not read %d of %d subscriptions",
			report.Unreadable, report.Checked)
	}
	return nil
}

// newPayments builds only what reconcile needs: the provider and the product
// map. No return URL — this pass never starts a checkout.
func newPayments(ctx context.Context, providerName string) (*corebilling.Payments, error) {
	// Normalised exactly as the server normalises it: PUG_BILLING_PROVIDER=DODO
	// must not start one and fail the other.
	name := strings.ToLower(strings.TrimSpace(providerName))
	if name == "" {
		return nil, nil
	}
	if name != dodo.Name {
		return nil, errors.New("unknown PUG_BILLING_PROVIDER " + providerName)
	}
	var dodoCfg dodo.Config
	if err := envconfig.Process(ctx, &dodoCfg); err != nil {
		return nil, err
	}
	products, err := dodo.ProductIDs(nil)
	if err != nil {
		return nil, err
	}
	client, err := dodo.New(dodoCfg, products)
	if err != nil {
		return nil, err
	}
	if client == nil {
		// A named provider with no API key is a misconfigured CronJob, not the self-hosted
		// shape: that one leaves PUG_BILLING_PROVIDER empty and returns above.
		return nil, errors.New("PUG_BILLING_PROVIDER is " + name + " but no API key is configured")
	}
	slugByProduct := make(map[string]string, len(products))
	for slug, id := range products {
		slugByProduct[id] = slug
	}
	return &corebilling.Payments{
		ProductBySlug: products,
		Provider:      client,
		SlugByProduct: slugByProduct,
	}, nil
}

func setupFailed(ctx context.Context, what string, err error) error {
	slog.ErrorContext(ctx, "billing reconcile setup failed", slogx.Error(err), slog.String("step", what))
	telemetry.RecordError(ctx, err)
	return err
}
