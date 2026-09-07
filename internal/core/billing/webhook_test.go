package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// fakeProvider is the whole seam, stubbed: the inbox, the CAS, attribution and the
// rejection dispositions are all exercised through it, which is what proves those
// paths hold no Dodo assumption.
type fakeProvider struct {
	name  string
	event corebilling.SubscriptionEvent
	err   error
	// The confirm-on-return path reads its own event, so a test can make the
	// provider disagree with what any delivery said.
	checkout    corebilling.SubscriptionEvent
	checkoutErr error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (f *fakeProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return f.event, f.err
}

func (f *fakeProvider) CreateCheckoutSession(context.Context, corebilling.CheckoutInput) (string, string, error) {
	return "cs_fake", "https://pay.example/checkout", nil
}

func (f *fakeProvider) CreatePortalSession(context.Context, string) (string, error) {
	return "https://pay.example/portal", nil
}

func (f *fakeProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return f.event, nil
}

func (f *fakeProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return f.checkout, f.checkoutErr
}

const fakeProviderName = "fake"

func newPaidFixture(t *testing.T) (*fixture, *fakeProvider) {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	org, err := dbwriteOrg(t, pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	provider := &fakeProvider{name: fakeProviderName}
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, &corebilling.Payments{
		ProductBySlug: map[string]string{"growth": "prod_growth", "scale": "prod_scale"},
		Provider:      provider,
		ReturnURL:     "https://app.example/settings/billing",
		SlugByProduct: map[string]string{"prod_growth": "growth", "prod_scale": "scale"},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &fixture{svc: svc, pg: pg, orgID: org}, provider
}

func subEvent(orgID, subID, product string, status corebilling.SubStatus) corebilling.SubscriptionEvent {
	return corebilling.SubscriptionEvent{
		Currency:           "USD",
		CurrentPeriodEnd:   time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second),
		CurrentPeriodStart: time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second),
		OrgID:              orgID,
		PriceCents:         2_000,
		ProductID:          product,
		ProviderCustomerID: "cus_" + orgID,
		ProviderStatus:     string(status),
		ProviderSubID:      subID,
		Status:             status,
	}
}

func delivery(id string, at time.Time) corebilling.Delivery {
	return corebilling.Delivery{
		DeliveredAt: at,
		EventType:   "subscription.active",
		RawPayload:  []byte(`{"type":"subscription.active","data":{}}`),
		WebhookID:   id,
	}
}

func storedDelivery(t *testing.T, f *fixture, id string) dbread.BillingWebhookDelivery {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select error, event_type, payload, processed_at, provider, received_at, webhook_id
		 from billing_webhook_deliveries where provider = $1 and webhook_id = $2`,
		fakeProviderName, id)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no delivery stored for %s", id)
	}
	var d dbread.BillingWebhookDelivery
	if err := rows.Scan(&d.Error, &d.EventType, &d.Payload, &d.ProcessedAt, &d.Provider,
		&d.ReceivedAt, &d.WebhookID); err != nil {
		t.Fatalf("scan delivery: %v", err)
	}
	return d
}

func TestDeliveryAppliesASubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" || ent.Status != corebilling.StatusActive {
		t.Errorf("entitlement = (%s, %s), want (growth, ACTIVE)", ent.Slug, ent.Status)
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub_status = %q, want active", ent.SubStatus)
	}
	if got := storedDelivery(t, f, "evt_1"); !got.ProcessedAt.Valid || got.Error != "" {
		t.Errorf("delivery processed=%v error=%q, want processed with no error", got.ProcessedAt.Valid, got.Error)
	}
}

// Deliveries are unordered and each carries the latest object, so an older one
// must not overwrite a newer.
func TestOutOfOrderDeliveryIsRefusedByTheCAS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	newer := time.Now().UTC().Truncate(time.Second)
	older := newer.Add(-time.Hour)

	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_new", newer)); err != nil {
		t.Fatalf("HandleDelivery(newer): %v", err)
	}
	// The active delivery is genuinely older, so it must lose even though it
	// arrives second.
	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_old", older)); err != nil {
		t.Fatalf("HandleDelivery(older): %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.SlugFree {
		t.Errorf("slug = %q, want free — a stale delivery revived a cancelled subscription", ent.Slug)
	}
	// Accepted, not retried: the provider is not at fault for delivering in any
	// order it likes.
	if d := storedDelivery(t, f, "evt_old"); !d.ProcessedAt.Valid {
		t.Error("a stale delivery was left unprocessed, so the provider will retry it forever")
	}
}

// SubStatus.Live() is Go; the same set is hardcoded in three SQL sites with nothing
// linking them, so adding a live status in Go alone would drop a paying org to the
// floor. This walks the whole vocabulary through the real query.
func TestTheLiveStatusSetAgreesBetweenGoAndSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	for _, status := range corebilling.AllSubStatuses() {
		t.Run(string(status), func(t *testing.T) {
			f, provider := newPaidFixture(t)
			provider.event = subEvent(f.orgID, "sub_1", "prod_growth", status)
			if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now().UTC())); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}

			ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
			if err != nil {
				t.Fatalf("GetEntitlement: %v", err)
			}
			// A live status supplies the subscription's plan; anything else leaves the
			// org on the derived floor.
			gotPlan := ent.Slug == "growth"
			if gotPlan != status.Live() {
				t.Errorf("status %q: resolved slug %q (supplies a plan = %v), but Live() = %v",
					status, ent.Slug, gotPlan, status.Live())
			}
		})
	}
}

// A live custom subscription resolves its quota from the entitlement row, so
// deleting it drops an org still being charged -- and reconcile looks for the inverse.
func TestClearIsRefusedUnderALiveCustomSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:          corebilling.SlugCustom,
		IncludedEvents:    new(int64(5_000_000)),
		ProviderProductID: new("prod_acme"),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_acme", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("evt_1", time.Now().UTC())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if err := f.svc.Clear(ctx, f.orgID, actor); !errors.Is(err, corebilling.ErrClearWouldStrandSubscription) {
		t.Fatalf("Clear under a live custom subscription: err = %v, want ErrClearWouldStrandSubscription", err)
	}
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if got := ent.IncludedEvents; got == nil || *got != 5_000_000 {
		t.Errorf("quota = %v, want the deal's 5000000 — the org is still being charged", got)
	}
}

// A webhook's CAS stamp is the whole-second webhook-timestamp header, so a
// cutover's cancellation and activation can carry the same one. A tie must not be
// able to grant a plan: the safe direction is withholding one.
func TestASameSecondDeliveryCannotReviveACancelledSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_cancel", at)); err != nil {
		t.Fatalf("HandleDelivery(cancelled): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", at)); err != nil {
		t.Fatalf("HandleDelivery(active): %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.SlugFree {
		t.Errorf("slug = %q, want free — a same-second delivery revived a cancelled subscription", ent.Slug)
	}
}

// A payload pug cannot decode is a shape that changed under it, which a redeploy
// fixes. Storing it processed would consume the delivery and lose the replay.
func TestAnUndecodablePayloadIsRetried(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.err = errors.New("dodo: decode subscription payload: json: cannot unmarshal")

	err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_garbled", time.Now().UTC()))
	if err == nil {
		t.Fatal("a payload pug could not decode was accepted, so the provider will never retry it")
	}
	if d := storedDelivery(t, f, "evt_garbled"); d.ProcessedAt.Valid {
		t.Error("an undecodable delivery was marked processed, consuming it permanently")
	}
}

// The provider's retry reuses its webhook id. A retry whose row is still
// unprocessed died mid-apply; a bare conflict->200 would neutralize it.
func TestRetryOfAnUnprocessedDeliveryReapplies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	// Simulate the attempt that died after the insert: the row exists, unprocessed.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries (event_type, payload, provider, webhook_id)
		 values ('subscription.active', '{}'::jsonb, $1, 'evt_1')`, fakeProviderName); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want growth — the retry did not re-apply", ent.Slug)
	}
}

// A retry of a delivery that DID apply must not run again.
func TestRetryOfAProcessedDeliveryIsANoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	// A different payload under the same webhook id: if the retry re-applied, the
	// entitlement would move.
	provider.event = subEvent(f.orgID, "sub_1", "prod_scale", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at.Add(time.Hour))); err != nil {
		t.Fatalf("HandleDelivery(retry): %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want growth — a processed delivery was applied twice", ent.Slug)
	}
}

// Every unapplicable delivery has one disposition: stored, marked processed with a
// reason, never retried -- retrying fixes none of them.
func TestUnapplicableDeliveriesAreAcceptedAndRecorded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cases := []struct {
		name   string
		event  func(orgID string) corebilling.SubscriptionEvent
		reason string
	}{
		{
			name: "foreign currency",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
				e.Currency = "EUR"
				return e
			},
			reason: "currency",
		},
		{
			name: "unmapped product",
			event: func(orgID string) corebilling.SubscriptionEvent {
				return subEvent(orgID, "sub_1", "prod_unknown", corebilling.SubStatusActive)
			},
			reason: "product",
		},
		{
			name: "unattributable",
			event: func(string) corebilling.SubscriptionEvent {
				e := subEvent("", "sub_1", "prod_growth", corebilling.SubStatusActive)
				e.OrgID = xid.New().String()
				e.ProviderCustomerID = "cus_nobody"
				return e
			},
			reason: "attribution",
		},
		{
			name: "no status",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
				e.Status = ""
				return e
			},
			reason: "status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			provider.event = tc.event(f.orgID)

			if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
				t.Fatalf("HandleDelivery returned an error, so the provider will retry: %v", err)
			}
			stored := storedDelivery(t, f, "evt_1")
			if !stored.ProcessedAt.Valid {
				t.Fatal("delivery left unprocessed; the provider will retry it forever")
			}
			if stored.Error == "" {
				t.Fatal("delivery recorded no reason for not applying")
			}
			if got := stored.Error; len(got) < len(tc.reason) || got[:len(tc.reason)] != tc.reason {
				t.Errorf("error = %q, want it to start with %q", got, tc.reason)
			}

			ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
			if err != nil {
				t.Fatalf("GetEntitlement: %v", err)
			}
			if ent.Slug != corebilling.SlugFree {
				t.Errorf("slug = %q, want free — an unapplicable delivery changed the entitlement", ent.Slug)
			}
		})
	}
}

// Attribution falls back to the provider customer when a delivery carries no
// org metadata — a renewal, say, which Dodo need not echo metadata onto.
func TestAttributionFallsBackToTheProviderCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	provider.event = subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	renewal := subEvent(f.orgID, "sub_1", "prod_growth", corebilling.SubStatusPastDue)
	renewal.OrgID = ""
	provider.event = renewal
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_2", time.Now().Add(time.Minute))); err != nil {
		t.Fatalf("HandleDelivery(renewal): %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.SubStatus != corebilling.SubStatusPastDue {
		t.Errorf("sub_status = %q, want past_due — the fallback did not attribute the renewal", ent.SubStatus)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want growth — past_due must keep the plan", ent.Slug)
	}
}

// One buyer paying for two orgs shares a provider customer, so the fallback has
// nothing to tell them apart: rejected rather than attributed to the newest.
func TestAmbiguousProviderCustomerIsNotAttributed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	other, err := dbwriteOrg(t, f.pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	for i, orgID := range []string{f.orgID, other} {
		event := subEvent(orgID, "sub_"+orgID, "prod_growth", corebilling.SubStatusActive)
		event.ProviderCustomerID = "cus_shared"
		provider.event = event
		if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_seed_"+orgID, time.Now())); err != nil {
			t.Fatalf("HandleDelivery(seed %d): %v", i, err)
		}
	}

	unattributed := subEvent("", "sub_new", "prod_scale", corebilling.SubStatusActive)
	unattributed.ProviderCustomerID = "cus_shared"
	provider.event = unattributed
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_new", time.Now().Add(time.Minute))); err != nil {
		t.Fatalf("HandleDelivery returned an error, so the provider will retry: %v", err)
	}

	stored := storedDelivery(t, f, "evt_new")
	if !stored.ProcessedAt.Valid {
		t.Fatal("delivery left unprocessed; the provider will retry it forever")
	}
	if !strings.HasPrefix(stored.Error, "attribution") {
		t.Errorf("error = %q, want it to start with %q", stored.Error, "attribution")
	}

	var n int
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select count(*) from billing_subscriptions where provider_sub_id = $1`, "sub_new").Scan(&n); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if n != 0 {
		t.Errorf("stored %d rows for the ambiguous subscription, want 0", n)
	}
}

// A negotiated deal's product is not in config; it is on the org's own row, and
// the quota comes from the same row.
func TestCustomProductResolvesFromTheOrgRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	quota := int64(5_000_000)
	productID := "prod_acme"
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:          corebilling.SlugCustom,
		IncludedEvents:    &quota,
		ProviderProductID: &productID,
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub_1", productID, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.SlugCustom {
		t.Errorf("slug = %q, want custom", ent.Slug)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != quota {
		t.Errorf("quota = %v, want %d — a deal's quota comes from pug, not the provider", ent.IncludedEvents, quota)
	}
}

// A body Postgres cannot parse is still stored: losing the delivery entirely is
// the one thing the inbox exists to prevent.
func TestUnparseableBodyIsStillStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = corebilling.SubscriptionEvent{}

	d := delivery("evt_1", time.Now())
	d.RawPayload = []byte{0x00, 0x01, 0xff}
	if err := f.svc.HandleDelivery(t.Context(), provider, d); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	stored := storedDelivery(t, f, "evt_1")
	var wrapped map[string]string
	if err := json.Unmarshal(stored.Payload, &wrapped); err != nil {
		t.Fatalf("stored payload is not JSON: %v", err)
	}
	if wrapped["raw_base64"] == "" {
		t.Error("an unparseable body was stored without its bytes")
	}
}

// dbwriteOrg creates a backdated org, so a test asserting a granted plan is not
// also fighting a live trial window.
func dbwriteOrg(t *testing.T, pg *testutil.TestPostgres) (string, error) {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		return "", err
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	return org.ID, nil
}

// svcWithProvider rebuilds the service against a different provider, sharing the
// fixture's pools so the seeded rows are the ones reconciled.
func (f *fixture) svcWithProvider(t *testing.T, provider corebilling.PaymentProvider) *corebilling.Service {
	t.Helper()
	svc, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, true, &corebilling.Payments{
		ProductBySlug: map[string]string{"growth": "prod_growth", "scale": "prod_scale"},
		Provider:      provider,
		SlugByProduct: map[string]string{"prod_growth": "growth", "prod_scale": "scale"},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// A cutover whose new subscription is delivered before the old one's cancellation.
// Rejecting it would leave the org with no live subscription, so it must retry.
func TestSecondLiveSubscriptionIsRetriedNotConsumed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ctx := t.Context()

	provider.event = subEvent(f.orgID, "sub00000000000000031", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_old", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub00000000000000032", "prod_scale", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_new", time.Now())); err == nil {
		t.Fatal("a second live subscription was accepted; the provider must be asked to retry")
	}

	// Unprocessed, so the retry re-applies rather than short-circuiting.
	row, err := dbwrite.New(f.pg.PgW).InsertBillingWebhookDelivery(ctx, dbwrite.InsertBillingWebhookDeliveryParams{
		EventType: "subscription.active", Payload: []byte(`{}`),
		Provider: fakeProviderName, WebhookID: "wh_new",
	})
	if err != nil {
		t.Fatalf("read back the delivery: %v", err)
	}
	if row.ProcessedAt.Valid {
		t.Error("the delivery was marked processed; the retry will now be a no-op")
	}

	// Once the cancellation lands, the retry succeeds.
	provider.event = subEvent(f.orgID, "sub00000000000000031", "prod_growth", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_cancel", time.Now())); err != nil {
		t.Fatalf("HandleDelivery(cancel): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub00000000000000032", "prod_scale", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_new", time.Now())); err != nil {
		t.Fatalf("the retry after the cancellation failed: %v", err)
	}
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "scale" {
		t.Errorf("slug = %q, want scale — the cutover completed", ent.Slug)
	}
}
