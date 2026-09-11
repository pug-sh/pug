package billinginvoice

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// Off, or with no provider, there is nothing to invoice: exit 0 without touching
// Postgres or the provider.
func TestRunExitsZeroWithNothingToInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	t.Setenv("PUG_BILLING_ENABLED", "false")
	// Named and unbuildable: a disabled pass must not reach the provider at all.
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing disabled = %v, want nil", err)
	}

	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with no provider = %v, want nil", err)
	}
}

// A CronJob's only success signal is the exit code, so a provider it cannot
// build has to fail rather than invoice nothing quietly.
func TestRunFailsOnAMisconfiguredProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("PUG_BILLING_ENABLED", "true")

	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if err := Run(t.Context()); err == nil {
		t.Fatal("an unknown PUG_BILLING_PROVIDER exited 0")
	}
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "")
	if err := Run(t.Context()); err == nil {
		t.Fatal("a named provider with no API key exited 0")
	}
}

// Contention is not failure: Run exits 0 so the CronJob stays green. That the
// locked-out pass does not run is cron.WithLock's own guarantee, pinned there.
func TestRunExitsZeroWhenAnotherPassHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	tx, err := pg.PgW.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	acquired, err := dbwrite.New(tx).TryCronLock(t.Context(), int64(cron.LockBillingInvoice))
	if err != nil || !acquired {
		t.Fatalf("take the lock: acquired=%v err=%v", acquired, err)
	}

	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with the lock held = %v, want nil", err)
	}
}

// The exit code is the only thing a CronJob reports, and nothing pinned it. A
// declined card or a period the meter has not reached is a finding; a provider
// that could not be reached, or a charge whose outcome is unknown, is not.
func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		report  corebilling.InvoiceReport
		wantErr bool
	}{
		{"a clean pass", corebilling.InvoiceReport{Closed: 3, Charged: 2, Settled: 1}, false},
		{"nothing to do", corebilling.InvoiceReport{}, false},
		{"a declined card", corebilling.InvoiceReport{Declined: 4}, false},
		{"a cancelled mandate", corebilling.InvoiceReport{MandateGone: 2}, false},
		{"a period the meter has not reached", corebilling.InvoiceReport{Held: 5}, false},
		{"a plan that cannot be priced", corebilling.InvoiceReport{Unpriceable: 1}, false},
		{"an unreadable provider", corebilling.InvoiceReport{Unreadable: 1}, true},
		// One org charging says nothing about the ones that did not.
		{"an unreadable provider beside a success", corebilling.InvoiceReport{Charged: 9, Unreadable: 1}, true},
		{"a charge whose outcome is unknown", corebilling.InvoiceReport{Ambiguous: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := exitErr(tc.report); (err != nil) != tc.wantErr {
				t.Errorf("exitErr(%+v) = %v, want error: %v", tc.report, err, tc.wantErr)
			}
		})
	}
}
