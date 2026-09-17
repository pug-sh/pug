package billinginvoice

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
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Installed before any test calls SetupSDK: the global instruments bind to the first
// provider set. Counters accumulate process-wide, so tests compare deltas.
var testMeterReader = sdkmetric.NewManualReader()

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMeterReader)))
	testutil.Main(m)
}

func invoiceCount(t *testing.T, status string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMeterReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "billing.invoices_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("billing.invoices_total has data type %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, _ := dp.Attributes.Value(attribute.Key("status")); v.AsString() == status {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// seedDueInvoice stores an open invoice already due, which a pass with no provider
// cannot charge.
func seedDueInvoice(t *testing.T, pg *pgxpool.Pool) {
	t.Helper()
	if _, err := pg.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, currency, event_count, id, lines, next_attempt_at,
		   org_id, period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
		 values (1000, '2026-08-10', '2026-09-10', 'USD', 0, $1, '[]', now() - interval '1 hour',
		         $2, '2026-09-10', '2026-08-10', 'custom', '{}', 'open', 1000, now())`,
		xid.New().String(), xid.New().String()); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
}

// A declined card, a stale meter or a deal with no card is a person's to fix; the
// CronJob goes red only when the pass itself could not do its work.
func TestFailureIsThePassNotACustomer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		report  report
		wantErr bool
	}{
		{"nothing to do", report{}, false},
		{"a clean pass", report{
			close: corebilling.CloseReport{Closed: 3}, charge: corebilling.ChargeReport{Charged: 2},
			settle: corebilling.SettleReport{Paid: 1}, pin: corebilling.PinReport{Pinned: 1},
		}, false},
		{"a declined card", report{charge: corebilling.ChargeReport{Declined: 4, Uncollectible: 1}}, false},
		{"a stale meter", report{close: corebilling.CloseReport{Held: 5}}, false},
		{"a deal awaiting a card", report{charge: corebilling.ChargeReport{AwaitingCard: 2, MandatePaused: 1}}, false},
		{"a payment to refund by hand", report{settle: corebilling.SettleReport{Duplicate: 1, AmountMismatch: 1}}, false},
		{"usage nothing bills", report{unbilled: 7}, false},
		{"ten mandates gone", report{charge: corebilling.ChargeReport{MandateGone: maxMandatesGone}}, false},
		{"eleven mandates gone", report{charge: corebilling.ChargeReport{MandateGone: maxMandatesGone + 1}}, true},
		{"a charge that could not read the provider", report{charge: corebilling.ChargeReport{Charged: 9, Unreadable: 1}}, true},
		{"a settle that could not read the provider", report{settle: corebilling.SettleReport{Unreadable: 1}}, true},
		{"a pin that could not reach the provider", report{pin: corebilling.PinReport{Unreadable: 1}}, true},
		{"a charge left unresolved", report{charge: corebilling.ChargeReport{Ambiguous: 1}}, true},
		{"a period dropped unbilled", report{close: corebilling.CloseReport{Dropped: 1}}, true},
		{"a period no card prices", report{close: corebilling.CloseReport{Unpriceable: 1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.report.failure(); (err != nil) != tc.wantErr {
				t.Errorf("failure() = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// The grace is the meter's window, resolved by the meter's own clamp.
func TestGraceReadsTheMetersEnvVar(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", 48 * time.Hour},
		{"5", 120 * time.Hour},
		{"-1", 48 * time.Hour},
	} {
		env := map[string]string{}
		if tc.env != "" {
			env["PUG_USAGE_RESCAN_DAYS"] = tc.env
		}
		var cfg config
		if err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
			Target: &cfg, Lookuper: envconfig.MapLookuper(env),
		}); err != nil {
			t.Fatalf("ProcessWith: %v", err)
		}
		if got := cfg.grace(); got != tc.want {
			t.Errorf("PUG_USAGE_RESCAN_DAYS=%q: grace = %s, want %s", tc.env, got, tc.want)
		}
	}
}

// Its CronJob runs before billing is switched on, so off must exit 0 without
// reaching a provider.
func TestRunExitsZeroWithBillingOff(t *testing.T) {
	t.Setenv("PUG_BILLING_ENABLED", "false")
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing disabled = %v, want nil", err)
	}
}

func TestRunFailsOnAMisconfiguredProvider(t *testing.T) {
	t.Setenv("PUG_BILLING_ENABLED", "true")

	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	if err := Run(t.Context()); err == nil {
		t.Error("an unknown PUG_BILLING_PROVIDER exited 0")
	}
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "")
	if err := Run(t.Context()); err == nil {
		t.Error("a named provider with no API key exited 0")
	}
}

// Contention exits 0, and the pass did not run: once the lock is free the same due
// invoice fails it.
func TestRunExitsZeroWhenAnotherPassHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	seedDueInvoice(t, pg.PgW)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", "")

	holder, err := pg.PgW.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if acquired, err := dbwrite.New(holder).TryCronLock(t.Context(), int64(cron.LockBillingInvoice)); err != nil || !acquired {
		t.Fatalf("take the lock: acquired=%v err=%v", acquired, err)
	}
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with the lock held = %v, want nil", err)
	}

	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if err := Run(t.Context()); err == nil {
		t.Error("Run with a due invoice and no provider exited 0")
	}
}

func TestAFailingPassStillEmitsItsCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	seedDueInvoice(t, pg.PgW)
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into usage_periods (event_count, org_id, period_end, period_start)
		 values ($1, $2, now() - interval '10 days', now() - interval '40 days')`,
		corebilling.CurrentCard().FreeEvents+1, org.ID); err != nil {
		t.Fatalf("seed usage period: %v", err)
	}
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	before := invoiceCount(t, "unbilled")
	if err := pass(t.Context(), svc, time.Now(), 48*time.Hour); err == nil {
		t.Fatal("pass with a due invoice and no provider returned nil")
	}
	if got := invoiceCount(t, "unbilled") - before; got != 1 {
		t.Errorf("unbilled counter moved by %d, want 1", got)
	}
}
