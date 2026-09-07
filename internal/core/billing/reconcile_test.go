package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// fetchProvider serves reconcile: the subscription the provider reports, keyed
// by id, plus a read that fails.
type fetchProvider struct {
	fakeProvider
	remote map[string]corebilling.SubscriptionEvent
	fail   bool
}

func (f *fetchProvider) FetchSubscription(_ context.Context, id string) (corebilling.SubscriptionEvent, error) {
	if f.fail {
		return corebilling.SubscriptionEvent{}, errors.New("provider unreachable")
	}
	return f.remote[id], nil
}

func seedLiveSubscription(t *testing.T, f *fixture, subID, slug string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, current_period_end, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', now() + interval '20 days', $1, $2, $3, 2000, $4,
		         'cus_1', 'active', $5, now() - interval '1 hour', 'active')`,
		subID, f.orgID, slug, fakeProviderName, subID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

// The pass exists for the delivery that never arrived: the provider says
// cancelled, pug still says active, and only a re-read closes the gap.
func TestReconcileAppliesAMissedCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000010", "growth")

	cancelled := subEvent(f.orgID, "sub00000000000000010", "prod_growth", corebilling.SubStatusCancelled)
	provider := &fetchProvider{
		fakeProvider: fakeProvider{name: fakeProviderName},
		remote:       map[string]corebilling.SubscriptionEvent{"sub00000000000000010": cancelled},
	}
	svc := f.svcWithProvider(t, provider)

	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Checked != 1 || report.Applied != 1 {
		t.Errorf("report = %+v, want 1 checked and 1 applied", report)
	}

	ent, err := svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.SlugFree {
		t.Errorf("slug = %q, want free — the missed cancellation was not applied", ent.Slug)
	}
}

// The report's view of an org entitled to a paid plan nobody is charged for. Not
// auto-fixed: writing to the money side from a guess is what this must not do.
func TestReconcileReportsAPaidEntitlementWithNoSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{PlanSlug: "growth"}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	svc := f.svcWithProvider(t, &fetchProvider{fakeProvider: *provider})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.EntitledUnbilled != 1 {
		t.Errorf("entitled_unbilled = %d, want 1", report.EntitledUnbilled)
	}

	// Still granted: the report is a report, not a repair.
	ent, err := svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want growth — reconcile must not revoke a grant", ent.Slug)
	}
}

// A provider outage must not read as "everything is consistent", and one
// unreadable subscription must not abandon the rest of the pass.
func TestReconcileCountsUnreadableSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000011", "growth")

	svc := f.svcWithProvider(t, &fetchProvider{fakeProvider: fakeProvider{name: fakeProviderName}, fail: true})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile returned an error; one unreadable row must not abandon the pass: %v", err)
	}
	if report.Unreadable != 1 || report.Applied != 0 {
		t.Errorf("report = %+v, want 1 unreadable and 0 applied", report)
	}
}

// A live subscription against a product nothing maps to: a deploy is missing a
// product key, or an operator made a product without pasting its id.
func TestReconcileReportsAnUnmappedProduct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000012", "growth")

	orphan := subEvent(f.orgID, "sub00000000000000012", "prod_nobody_knows", corebilling.SubStatusActive)
	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: fakeProvider{name: fakeProviderName},
		remote:       map[string]corebilling.SubscriptionEvent{"sub00000000000000012": orphan},
	})

	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.UnmappedProduct != 1 || report.Applied != 0 {
		t.Errorf("report = %+v, want 1 unmapped_product and 0 applied", report)
	}
}

// A deployment with no provider has nothing to reconcile against, which is the
// self-hosted shape and not an error.
func TestReconcileWithoutAProviderIsANoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	report, err := f.svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Checked != 0 {
		t.Errorf("checked = %d, want 0", report.Checked)
	}
}

// Payloads carry personal data pug does not otherwise store, and only replay needs
// the bytes. Unprocessed rows are never pruned -- those are still worth replaying.
func TestPruneKeepsUnprocessedDeliveries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	old := time.Now().Add(-corebilling.DeliveryRetention - 24*time.Hour)

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries (event_type, payload, processed_at, provider, webhook_id)
		 values ('subscription.active', '{}'::jsonb, $1, $2, 'evt_old'),
		        ('subscription.active', '{}'::jsonb, null, $2, 'evt_stuck')`,
		old, fakeProviderName); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}

	pruned, err := f.svc.PruneDeliveries(t.Context(), time.Now().Add(-corebilling.DeliveryRetention))
	if err != nil {
		t.Fatalf("PruneDeliveries: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 — only the processed row is past retention", pruned)
	}

	var remaining string
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select webhook_id from billing_webhook_deliveries where provider = $1`,
		fakeProviderName).Scan(&remaining); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if remaining != "evt_stuck" {
		t.Errorf("remaining delivery = %q, want evt_stuck", remaining)
	}
}
