// Package billing runs one payments reconcile pass: it re-reads every stored
// subscription from the provider, applies each through the same CAS the webhook
// uses, reports the inconsistencies it cannot fix, and prunes delivery payloads
// past their retention. Then it returns -- scheduling is the deployment's job (a
// k8s CronJob), not this process's.
//
// It is the backstop for the one thing the inbox cannot cover: a webhook that
// never arrived at all. Nothing here auto-repairs; an automatic fix would be
// writing to the money side of the system from a guess.
package billing

import (
	"context"
	"errors"
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

// passTimeout bounds one pass end to end. The pass holds an advisory lock for its
// whole duration, so a hang does not merely stall this run: every later pod finds
// the lock held and exits 0, leaving a green CronJob and a reconcile that has not
// run for as long as the wedged pod lives. Generous because the pass makes one
// provider round-trip per subscription.
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
	// Off is the self-hosted shape: no quota, no provider, nothing to reconcile.
	// Exit 0 rather than fail, so the CronJob is green on a deployment that simply
	// does not bill.
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
		// Another pod is doing this work. Exit 0 — alerting on healthy overlap would
		// alert on nothing — but say so, because "skipped" and "reconciled" are
		// otherwise the same silent success.
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
	if _, err := svc.Reconcile(ctx, now); err != nil {
		return err
	}
	// After the reconcile, not before: a delivery still worth replaying is one the
	// pass may have just made sense of.
	pruned, err := svc.PruneDeliveries(ctx, now.Add(-corebilling.DeliveryRetention))
	if err != nil {
		return err
	}
	if pruned > 0 {
		slog.InfoContext(ctx, "pruned billing webhook deliveries", slog.Int64("rows", pruned))
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
		return nil, nil
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
