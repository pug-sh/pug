package billing

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func newSvc(t *testing.T, pg *testutil.TestPostgres) *corebilling.Service {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// seedDelivery stores one processed delivery stamped at `at`.
func seedDelivery(t *testing.T, pg *pgxpool.Pool, webhookID string, at time.Time) {
	t.Helper()
	if _, err := pg.Exec(t.Context(),
		`insert into billing_webhook_deliveries (event_type, payload, processed_at, provider, webhook_id)
		 values ('subscription.active', '{}'::jsonb, $1, 'dodo', $2)`, at, webhookID); err != nil {
		t.Fatalf("seed delivery %s: %v", webhookID, err)
	}
}

func deliveryIDs(t *testing.T, pg *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pg.Query(t.Context(), `select webhook_id from billing_webhook_deliveries order by webhook_id`)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func holdLock(t *testing.T, pg *pgxpool.Pool) {
	t.Helper()
	tx, err := pg.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	acquired, err := dbwrite.New(tx).TryCronLock(t.Context(), int64(cron.LockBillingReconcile))
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !acquired {
		t.Fatal("could not take the reconcile lock to hold it")
	}
}

// The retention window, not merely "old": a sign error here deletes every
// delivery on the first pass.
func TestPassPrunesOnlyPastRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedDelivery(t, pg.PgW, "evt_expired", now.Add(-corebilling.DeliveryRetention-time.Hour))
	seedDelivery(t, pg.PgW, "evt_fresh", now.Add(-corebilling.DeliveryRetention+time.Hour))

	if err := pass(t.Context(), newSvc(t, pg), now); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := deliveryIDs(t, pg.PgRO)
	if len(got) != 1 || got[0] != "evt_fresh" {
		t.Errorf("deliveries = %v, want only evt_fresh", got)
	}
}

// Off is the self-hosted shape: a green CronJob on a deployment that does not
// bill, and one that never asks for a database.
func TestRunIsANoOpWhenBillingIsDisabled(t *testing.T) {
	t.Setenv("PUG_BILLING_ENABLED", "false")
	t.Setenv("DATABASE_URL", "")

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing disabled = %v, want nil", err)
	}
}

// A CronJob's only success signal is the exit code, so a named provider it
// cannot build has to fail rather than reconcile nothing quietly.
func TestRunFailsOnAnUnknownProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	if err := Run(t.Context()); err == nil {
		t.Fatal("an unknown PUG_BILLING_PROVIDER exited 0")
	}
}

func TestRunPrunesWithNoProviderConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	seedDelivery(t, pg.PgW, "evt_expired", time.Now().Add(-corebilling.DeliveryRetention-time.Hour))
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deliveryIDs(t, pg.PgRO); len(got) != 0 {
		t.Errorf("deliveries = %v, want the expired one pruned", got)
	}
}

// Contention is not failure -- but the pass must also not have run. Asserting the
// stale delivery survives is what proves the lock was respected rather than that
// nothing happened to go wrong.
func TestRunExitsZeroWhenAnotherPassHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	seedDelivery(t, pg.PgW, "evt_expired", time.Now().Add(-corebilling.DeliveryRetention-time.Hour))
	holdLock(t, pg.PgW)

	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with the lock held = %v, want nil (healthy overlap is not an alert)", err)
	}
	if got := deliveryIDs(t, pg.PgRO); len(got) != 1 {
		t.Errorf("deliveries = %v, want the pass skipped entirely", got)
	}
}

func TestNewPayments(t *testing.T) {
	t.Run("no provider named", func(t *testing.T) {
		p, err := newPayments(t.Context(), "")
		if err != nil || p != nil {
			t.Fatalf("newPayments = (%v, %v), want (nil, nil)", p, err)
		}
	})

	t.Run("an unknown provider is an error", func(t *testing.T) {
		if _, err := newPayments(t.Context(), "stripe"); err == nil {
			t.Fatal("an unknown provider was accepted")
		}
	})

	// Credentials absent is the same supported shape as no provider at all.
	t.Run("no api key", func(t *testing.T) {
		t.Setenv("PUG_DODO_API_KEY", "")
		p, err := newPayments(t.Context(), dodo.Name)
		if err != nil || p != nil {
			t.Fatalf("newPayments = (%v, %v), want (nil, nil)", p, err)
		}
	})

	t.Run("builds both directions of the product map", func(t *testing.T) {
		t.Setenv("PUG_DODO_API_KEY", "sk_test")
		t.Setenv("PUG_DODO_PRODUCT_GROWTH", "prod_growth")

		p, err := newPayments(t.Context(), dodo.Name)
		if err != nil {
			t.Fatalf("newPayments: %v", err)
		}
		if p == nil {
			t.Fatal("newPayments returned no provider for a configured deployment")
		}
		if p.ProductBySlug["growth"] != "prod_growth" || p.SlugByProduct["prod_growth"] != "growth" {
			t.Errorf("product map = %v / %v", p.ProductBySlug, p.SlugByProduct)
		}
		// This pass never starts a checkout, so there is nowhere to return a buyer to.
		if p.ReturnURL != "" {
			t.Errorf("ReturnURL = %q, want empty", p.ReturnURL)
		}
	})
}
