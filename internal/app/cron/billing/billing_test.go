package billing

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func newSvc(t *testing.T, pg *testutil.TestPostgres) *corebilling.Service {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, nil)
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

// unreachableProvider fails every re-read, which is what makes Reconcile report
// an unreadable subscription — the pass's failure exit.
type unreachableProvider struct{}

func (unreachableProvider) Name() string { return dodo.Name }

func (unreachableProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, errors.New("unused")
}

func (unreachableProvider) CanVerify() bool { return true }

func (unreachableProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errors.New("unused")
}

func (unreachableProvider) CreateCheckoutSession(context.Context, corebilling.CheckoutInput) (string, string, error) {
	return "", "", errors.New("unused")
}

func (unreachableProvider) CreatePortalSession(context.Context, string) (string, error) {
	return "", errors.New("unused")
}

func (unreachableProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errors.New("provider unreachable")
}

func (unreachableProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errors.New("unused")
}

func (unreachableProvider) NormalizePayment(corebilling.Delivery) (corebilling.PaymentEvent, error) {
	return corebilling.PaymentEvent{}, errors.New("unused")
}

func (unreachableProvider) Charge(context.Context, corebilling.ChargeInput) (string, error) {
	return "", errors.New("unused")
}

func (unreachableProvider) ListPayments(context.Context, string, time.Time) ([]corebilling.PaymentRecord, error) {
	return nil, errors.New("unused")
}

func (unreachableProvider) FetchPayment(context.Context, string) (corebilling.PaymentRecord, error) {
	return corebilling.PaymentRecord{}, errors.New("unused")
}

func (unreachableProvider) SetNextBillingDate(context.Context, string, time.Time) error {
	return errors.New("unused")
}

func (unreachableProvider) CancelSubscription(context.Context, string) error {
	return errors.New("unused")
}

// seedLiveSubscription gives the pass something to re-read, so a failing
// provider produces an unreadable report rather than an empty one.
func seedLiveSubscription(t *testing.T, pg *pgxpool.Pool) {
	t.Helper()
	org, err := dbwrite.New(pg).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := pg.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, current_period_end, id, on_demand, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', now() + interval '20 days', $1, true, $2, 'usage-2026-09', 0, $3,
		         'cus_1', 'active', 'sub_1', now() - interval '1 hour', 'active')`,
		xid.New().String(), org.ID, dodo.Name); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

// The prune sits ahead of that failure deliberately: a provider outage is no reason
// to keep an expired payload for as long as the outage lasts.
func TestPassPrunesEvenWhenTheProviderIsUnreadable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedDelivery(t, pg.PgW, "evt_expired", now.Add(-corebilling.DeliveryRetention-time.Hour))
	seedLiveSubscription(t, pg.PgW)

	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		Provider:       unreachableProvider{},
		MandateProduct: "prod_mandate",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if err := pass(t.Context(), svc, now); err == nil {
		t.Fatal("pass returned nil though the provider could not be read")
	}
	if got := deliveryIDs(t, pg.PgRO); len(got) != 0 {
		t.Errorf("deliveries = %v, want none — the failure skipped the prune", got)
	}
}

// Off means nothing to reconcile, not nothing to do: a deployment that took
// webhooks and then disabled billing still holds payloads with personal data in
// them, and this pass is the only thing that prunes them.
func TestRunPrunesWhenBillingIsDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	seedDelivery(t, pg.PgW, "evt_expired", time.Now().Add(-corebilling.DeliveryRetention-time.Hour))
	seedDelivery(t, pg.PgW, "evt_recent", time.Now())
	t.Setenv("PUG_BILLING_ENABLED", "false")
	// Named and unbuildable: a disabled pass must not reach the provider at all.
	t.Setenv("PUG_BILLING_PROVIDER", "stripe")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing disabled = %v, want nil", err)
	}
	if got := deliveryIDs(t, pg.PgRO); len(got) != 1 || got[0] != "evt_recent" {
		t.Errorf("deliveries = %v, want only evt_recent", got)
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

// The other half: a named provider with no credentials reconciles nothing, and
// exit 0 would report that as a clean pass. The server degrades here instead.
func TestRunFailsOnANamedProviderWithNoAPIKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("PUG_BILLING_ENABLED", "true")
	t.Setenv("PUG_BILLING_PROVIDER", dodo.Name)
	t.Setenv("PUG_DODO_API_KEY", "")
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	if err := Run(t.Context()); err == nil {
		t.Fatal("a named provider with no API key exited 0")
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

// Contention is not failure — but the pass must also not have run. The stale
// delivery surviving is what proves the lock was respected.
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
