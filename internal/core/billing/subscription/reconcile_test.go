package subscription_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/subscription"
	"github.com/rs/xid"
)

// fetchProvider serves reconcile: the subscription the provider reports, keyed
// by id, plus a read that fails.
type fetchProvider struct {
	fakeProvider
	remote map[string]corebilling.SubscriptionEvent
	fail   bool
	// fetchErr is the specific failure, for the dispositions the pass tells apart.
	fetchErr error
}

func (f *fetchProvider) FetchSubscription(_ context.Context, id string) (corebilling.SubscriptionEvent, error) {
	if f.fetchErr != nil {
		return corebilling.SubscriptionEvent{}, f.fetchErr
	}
	if f.fail {
		return corebilling.SubscriptionEvent{}, errors.New("provider unreachable")
	}
	return f.remote[id], nil
}

func seedLiveSubscription(t *testing.T, f *fixture, subID, slug string) {
	t.Helper()
	seedSubscription(t, f, subID, slug, "active")
}

func seedSubscription(t *testing.T, f *fixture, subID, slug, status string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, current_period_end, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', now() + interval '20 days', $1, $2, $3, 2000, $4,
		         'cus_1', $6, $5, now() - interval '1 hour', $6)`,
		subID, f.orgID, slug, fakeProviderName, subID, status); err != nil {
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
	seedLiveSubscription(t, f, "sub00000000000000010", entitlement.SlugUsage)

	cancelled := subEvent(f.orgID, "sub00000000000000010", "prod_u", corebilling.SubStatusCancelled)
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

	ent, err := f.entitlements.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != entitlement.SlugFree {
		t.Errorf("slug = %q, want free — the missed cancellation was not applied", ent.Slug)
	}
}

// The report's view of a deal nobody has bought yet, or whose subscription lapsed.
// Not auto-fixed: writing to the money side from a guess is what this must not do.
// A free row is ordinary and never walked.
func TestReconcileReportsADealWithNoSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	if _, err := f.entitlements.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	freeOrg := dbwriteOrg(t, f.pg)
	if _, err := f.entitlements.SetPlan(t.Context(), freeOrg, actor, entitlement.Change{PlanSlug: entitlement.SlugFree}); err != nil {
		t.Fatalf("SetPlan free: %v", err)
	}

	svc := f.svcWithProvider(t, &fetchProvider{fakeProvider: *provider})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.EntitledUnbilled != 1 {
		t.Errorf("entitled_unbilled = %d, want 1 (the deal, not the free org)", report.EntitledUnbilled)
	}

	// Still staged: the report is a report, not a repair.
	rec, err := f.entitlements.StoredRecord(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("StoredRecord: %v", err)
	}
	if rec.PlanSlug != entitlement.SlugCustom || rec.ProviderProductID != "prod_deal" {
		t.Errorf("stored %+v, want the deal untouched — reconcile must not revoke it", rec)
	}
}

// past_due is live in the unbilled walk too, not only in Live(): narrowing that
// join to 'active' would report every org whose card failed as entitled to a plan
// nobody is charged for, on every pass.
func TestPastDueCountsAsBilled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	if _, err := f.entitlements.SetPlan(t.Context(), f.orgID, actor, entitlement.Change{
		PlanSlug: entitlement.SlugCustom, ProviderProductID: new("prod_deal"),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	pastDue := subEvent(f.orgID, "sub00000000000000070", "prod_deal", corebilling.SubStatusPastDue)
	provider.event = pastDue
	if err := f.svc.HandleDelivery(t.Context(), delivery("wh_past_due_billed", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: *provider,
		remote:       map[string]corebilling.SubscriptionEvent{"sub00000000000000070": pastDue},
	})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.EntitledUnbilled != 0 {
		t.Errorf("entitled_unbilled = %d, want 0 — a past_due subscription is still being billed", report.EntitledUnbilled)
	}
}

// A provider outage must not read as "everything is consistent", and one
// unreadable subscription must not abandon the rest of the pass.
func TestReconcileCountsUnreadableSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000011", entitlement.SlugUsage)

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
	seedLiveSubscription(t, f, "sub00000000000000012", entitlement.SlugUsage)

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
// the bytes. A delivery still inside the window is kept whether it processed or
// not; one that never processed and is past it goes too, or an undecodable body
// would hold its payload forever.
func TestPruneDropsEverythingPastTheWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	past := time.Now().Add(-subscription.DeliveryRetention - 24*time.Hour)

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries
		   (event_type, payload, processed_at, provider, received_at, webhook_id)
		 values ('subscription.active', '{}'::jsonb, $1, $2, $1, 'evt_old'),
		        ('subscription.active', '{}'::jsonb, null, $2, $1, 'evt_abandoned'),
		        ('subscription.active', '{}'::jsonb, null, $2, now(), 'evt_stuck')`,
		past, fakeProviderName); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_checkout_sessions (create_time, org_id, provider, ref)
		 values ($1, $2, $3, 'ref_abandoned')`,
		past, f.orgID, fakeProviderName); err != nil {
		t.Fatalf("seed checkout session: %v", err)
	}

	pruned, err := f.svc.PruneDeliveries(t.Context(), time.Now().Add(-subscription.DeliveryRetention))
	if err != nil {
		t.Fatalf("PruneDeliveries: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2 — both rows past retention, processed or not", pruned)
	}

	var remaining string
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select webhook_id from billing_webhook_deliveries where provider = $1`,
		fakeProviderName).Scan(&remaining); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if remaining != "evt_stuck" {
		t.Errorf("remaining delivery = %q, want evt_stuck — a retry can still fix that one", remaining)
	}

	// The same window drops spent refs. Deleting one early makes its checkout
	// unattributable, so only the old one may go.
	var refs []string
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select ref from billing_checkout_sessions order by ref`)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan ref: %v", err)
		}
		refs = append(refs, ref)
	}
	if len(refs) != 1 || refs[0] != checkoutRef(f.orgID) {
		t.Errorf("refs = %v, want only the fresh %q", refs, checkoutRef(f.orgID))
	}
}

// A provider state no writer can store has to land on a counter: reported as a
// plain skip it would be neither an apply nor a finding, and the pass would
// print a sweep it did not make.
func TestReconcileCountsASubscriptionItCannotStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000013", entitlement.SlugUsage)

	broken := subEvent(f.orgID, "sub00000000000000013", "prod_u", corebilling.SubStatusActive)
	broken.ProviderCustomerID = ""
	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: fakeProvider{name: fakeProviderName},
		remote:       map[string]corebilling.SubscriptionEvent{"sub00000000000000013": broken},
	})

	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Unapplicable != 1 || report.Applied != 0 || report.Unreadable != 0 {
		t.Errorf("report = %+v, want 1 unapplicable and nothing else", report)
	}
}

// A read that SUCCEEDS and decodes to nothing is a payload shape pug no longer
// understands. Counted Untracked it would exit 0 on a pass that verified nothing,
// which is exactly the case reconcile exists to catch.
func TestReconcileCountsAnUndecodableSubscriptionAsUnreadable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000011", entitlement.SlugUsage)

	// An empty remote map: the fetch succeeds and yields a zero event.
	svc := f.svcWithProvider(t, &fetchProvider{fakeProvider: fakeProvider{name: fakeProviderName}})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Unreadable != 1 || report.Untracked != 0 {
		t.Errorf("report = %+v, want 1 unreadable and 0 untracked", report)
	}
}

// A delivery pug accepted and did not apply wrote no subscription row, so the walk
// cannot see it. Without this counter nothing reports it at all.
func TestReconcileCountsAcceptedButUnappliedDeliveries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries
		   (error, event_type, payload, processed_at, provider, received_at, webhook_id)
		 values ('product: no such product', 'subscription.active', '{}'::jsonb,
		         now(), $1, now(), 'evt_rejected'),
		        ('', 'subscription.active', '{}'::jsonb, now(), $1, now(), 'evt_applied')`,
		fakeProviderName); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}

	report, err := f.svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Rejected != 1 {
		t.Errorf("rejected = %d, want 1 — only the delivery carrying a reason", report.Rejected)
	}
}

// A delivery whose retries all failed carries no error and no processed_at, so
// Rejected cannot see it and it wrote no subscription row for the walk to find.
// Without this counter it is invisible to everything until the payload is pruned.
func TestReconcileCountsStrandedDeliveries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries
		   (event_type, payload, processed_at, provider, received_at, webhook_id)
		 values ('subscription.active', '{}'::jsonb, null, $1, now() - interval '2 days', 'evt_stranded'),
		        ('subscription.active', '{}'::jsonb, null, $1, now(), 'evt_in_flight'),
		        ('subscription.active', '{}'::jsonb, now(), $1, now() - interval '2 days', 'evt_done')`,
		fakeProviderName); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}

	report, err := f.svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Stranded != 1 {
		t.Errorf("stranded = %d, want 1 — a delivery still in flight is not stranded", report.Stranded)
	}
	if report.Rejected != 0 {
		t.Errorf("rejected = %d, want 0 — a stranded delivery carries no reason", report.Rejected)
	}
}

// The walk pages, and every other test seeds one row. An off-by-one here would
// reconcile the first page and exit 0, reporting an estate it never read.
func TestReconcileWalksPastThePageBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	const rows = 501

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 select 'USD', 'sub' || n, $1, 'usage-2026-10', 100, $2, 'cus_1', 'cancelled',
		        'sub' || n, now() - interval '1 hour', 'cancelled'
		 from generate_series(1, $3) n`,
		f.orgID, fakeProviderName, rows); err != nil {
		t.Fatalf("seed subscriptions: %v", err)
	}

	provider := &fetchProvider{fakeProvider: fakeProvider{name: fakeProviderName}}
	report, err := f.svcWithProvider(t, provider).Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Checked != rows {
		t.Errorf("checked = %d, want %d — the walk stopped at a page boundary", report.Checked, rows)
	}
}

// A finding, not an outage: counted apart from Unreadable, or it holds the
// CronJob red on every later run.
func TestReconcileCountsASubscriptionTheProviderDoesNotKnow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000040", entitlement.SlugUsage)

	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: fakeProvider{name: fakeProviderName},
		fetchErr:     fmt.Errorf("%w: sub00000000000000040", corebilling.ErrSubscriptionNotFound),
	})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Untracked != 1 || report.Unreadable != 0 {
		t.Errorf("report = %+v, want 1 untracked and 0 unreadable", report)
	}
}

// A cancellation that never landed, on an org that has since bought again.
// Counted, because it means an org may be billed twice.
func TestReconcileCountsTwoLiveSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedSubscription(t, f, "sub00000000000000050", entitlement.SlugUsage, "cancelled")
	seedLiveSubscription(t, f, "sub00000000000000051", entitlement.SlugUsage)

	// The provider still calls the old one active, so it collides with the live row.
	revived := subEvent(f.orgID, "sub00000000000000050", "prod_u", corebilling.SubStatusActive)
	current := subEvent(f.orgID, "sub00000000000000051", "prod_u", corebilling.SubStatusActive)
	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: fakeProvider{name: fakeProviderName},
		remote: map[string]corebilling.SubscriptionEvent{
			"sub00000000000000050": revived,
			"sub00000000000000051": current,
		},
	})

	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.TwoLive != 1 {
		t.Errorf("report = %+v, want 1 two_live", report)
	}

	// The org stays on the subscription it is actually charged for. Both are on the
	// usage plan, so the live row's id is the evidence.
	var live string
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select provider_sub_id from billing_subscriptions where org_id = $1 and status in ('active', 'past_due')`,
		f.orgID).Scan(&live); err != nil {
		t.Fatalf("read the live subscription: %v", err)
	}
	if live != "sub00000000000000051" {
		t.Errorf("live subscription = %s, want sub00000000000000051 — the revived row won", live)
	}
}

// cancellingProvider cancels from inside the first fetch: a CronJob deadline
// expiring mid-walk.
type cancellingProvider struct {
	fakeProvider
	cancel context.CancelFunc
	calls  int
}

func (c *cancellingProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	c.calls++
	c.cancel()
	return corebilling.SubscriptionEvent{}, nil
}

// Without this every remaining row turns one cancellation into a per-row provider
// error, reporting an outage that never happened.
func TestReconcileStopsOnACancelledContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedLiveSubscription(t, f, "sub00000000000000060", entitlement.SlugUsage)
	seedSubscription(t, f, "sub00000000000000061", entitlement.SlugUsage, "cancelled")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider := &cancellingProvider{fakeProvider: fakeProvider{name: fakeProviderName}, cancel: cancel}

	report, err := f.svcWithProvider(t, provider).Reconcile(ctx, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider was called %d times, want 1 — the walk carried on past the cancellation", provider.calls)
	}
	if report.Unreadable > 1 {
		t.Errorf("unreadable = %d; a cancellation was counted as a provider outage", report.Unreadable)
	}
}

// meteringCheck is a UsageMeter whose check finds one product misconfigured and
// cannot reach another, and records the tier count each check was held to.
type meteringCheck struct {
	bad, unreachable string
	tiers            []int
}

func (*meteringCheck) IngestUsage(context.Context, corebilling.UsageStatement) error { return nil }

func (m *meteringCheck) VerifyMetering(_ context.Context, tiers int, products []string) error {
	m.tiers = append(m.tiers, tiers)
	switch {
	case slices.Contains(products, m.bad):
		return fmt.Errorf("%w: does not attach tier 2", corebilling.ErrMeteringMisconfigured)
	case slices.Contains(products, m.unreachable):
		return errors.New("dodo: get product: 503 Service Unavailable")
	}
	return nil
}

// A deal's product is built by hand, so a tier's meter can be missing from it and
// that tier bills nothing, silently. Reconcile is where an operator hears of it.
func TestReconcileFlagsADealThatDoesNotBillEveryTier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id, base_plan_slug) values ($1, 'custom', 'prod_bad', $2)`,
		f.orgID, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed deal: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, id, org_id, plan_slug, price_cents,
		   provider, provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', now() + interval '20 days', $1, $2, 'custom', 100, $3, 'cus_1', 'active', 'sub_1',
		         now() - interval '1 hour', 'active')`,
		xid.New().String(), f.orgID, fakeProviderName); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	check := &meteringCheck{bad: "prod_bad"}
	svc := f.svcWithMeter(t, &fakeProvider{name: fakeProviderName}, check)
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.MisconfiguredDeals != 1 {
		t.Fatalf("MisconfiguredDeals = %d, want 1", report.MisconfiguredDeals)
	}
	// The fake cannot re-read the subscription itself, which the walk counts
	// unreadable whatever the deal check finds.
	walkUnreadable := report.Unreadable
	// Held to the tier count of the plan the deal is pinned to, not merely called.
	plan, _ := entitlement.PlanBySlug(entitlement.SlugUsage)
	if !slices.Equal(check.tiers, []int{plan.Tiers()}) {
		t.Fatalf("checked with tier counts %v, want [%d]", check.tiers, plan.Tiers())
	}

	// Pinned to a plan the catalog does not know, a deal has no tier count to check
	// its product against, however that product is built: the meter cannot split it.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set base_plan_slug = 'usage-2019-01' where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("repin the deal: %v", err)
	}
	unknown := f.svcWithMeter(t, &fakeProvider{name: fakeProviderName}, &meteringCheck{bad: "prod_other"})
	if report, err = unknown.Reconcile(t.Context(), time.Now()); err != nil || report.MisconfiguredDeals != 1 {
		t.Fatalf("report = %+v, err = %v; want the deal on an unknown plan flagged", report, err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set base_plan_slug = $2 where org_id = $1`, f.orgID, entitlement.SlugUsage); err != nil {
		t.Fatalf("repin the deal: %v", err)
	}

	// The same deal on a product that bills every tier is no finding.
	good := f.svcWithMeter(t, &fakeProvider{name: fakeProviderName}, &meteringCheck{bad: "prod_other"})
	if report, err = good.Reconcile(t.Context(), time.Now()); err != nil || report.MisconfiguredDeals != 0 {
		t.Fatalf("report = %+v, err = %v; want no misconfigured deal", report, err)
	}

	// A check the provider did not answer found nothing either way: counted as a
	// read that failed, which fails the CronJob, not as a misconfigured deal.
	down := f.svcWithMeter(t, &fakeProvider{name: fakeProviderName}, &meteringCheck{unreachable: "prod_bad"})
	if report, err = down.Reconcile(t.Context(), time.Now()); err != nil ||
		report.MisconfiguredDeals != 0 || report.Unreadable != walkUnreadable+1 {
		t.Fatalf("report = %+v, err = %v; want the unreachable check counted unreadable", report, err)
	}
}
