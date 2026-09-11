package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// fakeProvider is the whole seam, stubbed: the inbox, the CAS, attribution, the
// rejection dispositions and the charge state machine are all exercised through
// it, which is what proves those paths hold no Dodo assumption.
type fakeProvider struct {
	name  string
	event corebilling.SubscriptionEvent
	err   error
	// payment is what NormalizePayment returns for a non-subscription delivery.
	payment corebilling.PaymentEvent
	// The confirm-on-return path reads its own event, so a test can make the
	// provider disagree with what any delivery said.
	checkout    corebilling.SubscriptionEvent
	checkoutErr error
	checkoutIn  corebilling.CheckoutInput

	mu sync.Mutex
	// charge decides a charge's outcome; nil creates a succeeded-later payment.
	charge  func(corebilling.ChargeInput) (string, error)
	charges []corebilling.ChargeInput
	// payments is what ListPayments and FetchPayment read.
	payments  []corebilling.PaymentRecord
	listErr   error
	pinned    []time.Time
	cancelled []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (f *fakeProvider) CanVerify() bool { return true }

func (f *fakeProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return f.event, f.err
}

func (f *fakeProvider) NormalizePayment(corebilling.Delivery) (corebilling.PaymentEvent, error) {
	return f.payment, nil
}

func (f *fakeProvider) CreateCheckoutSession(
	_ context.Context, in corebilling.CheckoutInput,
) (string, string, error) {
	f.checkoutIn = in
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

func (f *fakeProvider) Charge(_ context.Context, in corebilling.ChargeInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charges = append(f.charges, in)
	if f.charge != nil {
		return f.charge(in)
	}
	id := fmt.Sprintf("pay_%d", len(f.charges))
	f.payments = append(f.payments, corebilling.PaymentRecord{PaymentID: id, InvoiceID: in.InvoiceID})
	return id, nil
}

// recordPayment stores a payment as the provider would after an ambiguous call.
func (f *fakeProvider) recordPayment(in corebilling.ChargeInput, status corebilling.PaymentStatus) string {
	id := fmt.Sprintf("pay_%d", len(f.charges))
	f.payments = append(f.payments, corebilling.PaymentRecord{PaymentID: id, InvoiceID: in.InvoiceID, Status: status})
	return id
}

func (f *fakeProvider) settlePayment(id string, status corebilling.PaymentStatus, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.payments {
		if f.payments[i].PaymentID == id {
			f.payments[i].Status = status
			f.payments[i].ErrorCode = code
			f.payments[i].InvoiceURL = "https://pay.example/receipt/" + id
		}
	}
}

func (f *fakeProvider) ListPayments(context.Context, string, time.Time) ([]corebilling.PaymentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]corebilling.PaymentRecord(nil), f.payments...), nil
}

func (f *fakeProvider) FetchPayment(_ context.Context, id string) (corebilling.PaymentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.payments {
		if p.PaymentID == id {
			return p, nil
		}
	}
	return corebilling.PaymentRecord{}, errors.New("no such payment")
}

func (f *fakeProvider) SetNextBillingDate(_ context.Context, _ string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, at)
	return nil
}

func (f *fakeProvider) CancelSubscription(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return nil
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
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       provider,
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	f := &fixture{svc: svc, pg: pg, orgID: org}
	seedCheckoutRef(t, f, org)
	return f, provider
}

// checkoutRef is the ref a delivery for orgID carries. Attribution is by ref, so a
// test org needs a stored checkout to be reachable at all.
func checkoutRef(orgID string) string {
	if orgID == "" {
		return ""
	}
	return "ref_" + orgID
}

// seedCheckoutRef stands in for the row CreateCheckoutSession writes, pinned to
// the current card.
func seedCheckoutRef(t *testing.T, f *fixture, orgID string) {
	t.Helper()
	seedCheckoutRefFor(t, f, orgID, corebilling.CurrentSlug)
}

func seedCheckoutRefFor(t *testing.T, f *fixture, orgID, planSlug string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_checkout_sessions (org_id, plan_slug, provider, ref) values ($1, $2, $3, $4)`,
		orgID, planSlug, fakeProviderName, checkoutRef(orgID)); err != nil {
		t.Fatalf("seed checkout session: %v", err)
	}
}

func subEvent(orgID, subID, product string, status corebilling.SubStatus) corebilling.SubscriptionEvent {
	return corebilling.SubscriptionEvent{
		CheckoutRef:        checkoutRef(orgID),
		Currency:           "USD",
		CurrentPeriodEnd:   time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second),
		CurrentPeriodStart: time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second),
		OnDemand:           true,
		OrgID:              orgID,
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

// liveSubID is the provider id of the org's live mandate, or "" for none.
func liveSubID(t *testing.T, f *fixture) string {
	t.Helper()
	var id string
	err := f.pg.PgRO.QueryRow(t.Context(),
		`select provider_sub_id from billing_subscriptions
		 where org_id = $1 and status in ('active', 'past_due')`, f.orgID).Scan(&id)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("read live subscription: %v", err)
	}
	return id
}

func TestDeliveryAppliesASubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.CurrentSlug || ent.Status != corebilling.StatusActive || !ent.Chargeable {
		t.Errorf("entitlement = (%s, %s, chargeable=%v), want the pinned card, ACTIVE and chargeable",
			ent.Slug, ent.Status, ent.Chargeable)
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

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_new", newer)); err != nil {
		t.Fatalf("HandleDelivery(newer): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_old", older)); err != nil {
		t.Fatalf("HandleDelivery(older): %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Chargeable {
		t.Error("a stale delivery revived a cancelled mandate")
	}
	// Accepted, not retried: the provider is not at fault for delivering in any
	// order it likes.
	if d := storedDelivery(t, f, "evt_old"); !d.ProcessedAt.Valid {
		t.Error("a stale delivery was left unprocessed, so the provider will retry it forever")
	}
}

// SubStatus.Live() is Go; the same set is hardcoded in the SQL with nothing
// linking them, so adding a live status in Go alone would drop a paying org.
func TestTheLiveStatusSetAgreesBetweenGoAndSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if got := len(corebilling.AllSubStatuses()); got != 6 {
		t.Fatalf("vocabulary has %d statuses, want 6", got)
	}
	for _, status := range corebilling.AllSubStatuses() {
		t.Run(string(status), func(t *testing.T) {
			f, provider := newPaidFixture(t)
			provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", status)
			if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now().UTC())); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}
			ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
			if err != nil {
				t.Fatalf("GetEntitlement: %v", err)
			}
			if ent.Chargeable != status.Live() {
				t.Errorf("status %q: chargeable = %v, but Live() = %v", status, ent.Chargeable, status.Live())
			}
		})
	}
}

// past_due sits in the one-live index, not only in Live().
func TestPastDueHoldsTheOneLiveSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedSubscription(t, f, "sub00000000000000060", corebilling.CurrentSlug, "past_due")

	provider.event = subEvent(f.orgID, "sub00000000000000061", "prod_mandate", corebilling.SubStatusActive)
	err := f.svc.HandleDelivery(t.Context(), provider, delivery("wh_past_due", time.Now()))
	if !errors.Is(err, corebilling.ErrTwoLiveSubscriptions) {
		t.Fatalf("err = %v, want ErrTwoLiveSubscriptions — past_due did not hold the slot", err)
	}
}

// Clearing a deal under a live mandate is legal now: every org authorizes the
// same product, so the org lands on the current card rather than on nothing.
func TestClearUnderALiveMandateFallsToTheCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(40_000)),
	}); err != nil {
		t.Fatalf("set the deal: %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("evt_1", time.Now().UTC())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if err := f.svc.Clear(ctx, f.orgID, actor); err != nil {
		t.Fatalf("Clear under a live mandate: %v", err)
	}
	ent, err := f.svc.GetEntitlement(ctx, f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.CurrentSlug || !ent.Chargeable {
		t.Errorf("entitlement = %q chargeable=%v, want the current card, still chargeable", ent.Slug, ent.Chargeable)
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

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_cancel", at)); err != nil {
		t.Fatalf("HandleDelivery(cancelled): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", at)); err != nil {
		t.Fatalf("HandleDelivery(active): %v", err)
	}
	if liveSubID(t, f) != "" {
		t.Error("a same-second delivery revived a cancelled subscription")
	}
}

// The other half of the tie-break: a same-second cancellation ends an active one.
func TestASameSecondCancellationEndsAnActiveSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", at)); err != nil {
		t.Fatalf("HandleDelivery(active): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_cancel", at)); err != nil {
		t.Fatalf("HandleDelivery(cancelled): %v", err)
	}
	if liveSubID(t, f) != "" {
		t.Error("a same-second cancellation was dropped")
	}
	if d := storedDelivery(t, f, "evt_cancel"); !d.ProcessedAt.Valid || d.Error != "" {
		t.Errorf("delivery processed=%v error=%q, want processed with no error", d.ProcessedAt.Valid, d.Error)
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

	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_webhook_deliveries (event_type, payload, provider, webhook_id)
		 values ('subscription.active', '{}'::jsonb, $1, 'evt_1')`, fakeProviderName); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if liveSubID(t, f) != "sub_1" {
		t.Error("the retry did not re-apply")
	}
}

// A retry of a delivery that DID apply must not run again.
func TestRetryOfAProcessedDeliveryIsANoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	// A different payload under the same webhook id: if the retry re-applied, the
	// mandate would end.
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at.Add(time.Hour))); err != nil {
		t.Fatalf("HandleDelivery(retry): %v", err)
	}
	if liveSubID(t, f) != "sub_1" {
		t.Error("a processed delivery was applied twice")
	}
}

// Every unapplicable delivery has one disposition: stored, marked processed with a
// reason, never retried — retrying fixes none of them.
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
				e := subEvent(orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.Currency = "EUR"
				return e
			},
			reason: "currency",
		},
		{
			name: "unattributable",
			event: func(string) corebilling.SubscriptionEvent {
				e := subEvent("", "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.OrgID = xid.New().String()
				e.ProviderCustomerID = "cus_nobody"
				return e
			},
			reason: "attribution",
		},
		{
			name: "no status",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.Status = ""
				return e
			},
			reason: "status",
		},
		{
			name: "no customer",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.ProviderCustomerID = ""
				return e
			},
			reason: "customer",
		},
		{
			name: "negative price",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.PriceCents = -1
				return e
			},
			reason: "price",
		},
		{
			// A recurring subscription would be charged by the provider AND by pug.
			name: "not on-demand",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
				e.OnDemand = false
				return e
			},
			reason: "on_demand",
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
			if !strings.HasPrefix(stored.Error, tc.reason) {
				t.Errorf("error = %q, want it to start with %q", stored.Error, tc.reason)
			}
			if liveSubID(t, f) != "" {
				t.Error("an unapplicable delivery stored a live mandate")
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

	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	renewal := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusPastDue)
	renewal.CheckoutRef = ""
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
	if ent.Slug != corebilling.CurrentSlug || !ent.Chargeable {
		t.Errorf("slug = %q chargeable=%v, want the pinned card, still chargeable", ent.Slug, ent.Chargeable)
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
	seedCheckoutRef(t, f, other)

	for i, orgID := range []string{f.orgID, other} {
		event := subEvent(orgID, "sub_"+orgID, "prod_mandate", corebilling.SubStatusActive)
		event.ProviderCustomerID = "cus_shared"
		provider.event = event
		if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_seed_"+orgID, time.Now())); err != nil {
			t.Fatalf("HandleDelivery(seed %d): %v", i, err)
		}
	}

	unattributed := subEvent("", "sub_new", "prod_mandate", corebilling.SubStatusActive)
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

// A deal's terms are pug's; the mandate only makes them chargeable.
func TestCustomDealResolvesWithAMandate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		BlockRateCents: new(int64(300)),
		IncludedEvents: new(int64(5_000_000)),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.SlugCustom || ent.Terms == nil || !ent.Chargeable {
		t.Errorf("entitlement = %q terms=%v chargeable=%v, want custom, terms, chargeable", ent.Slug, ent.Terms, ent.Chargeable)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != 5_000_000 {
		t.Errorf("quota = %v, want 5000000 — a deal's allowance comes from pug, not the provider", ent.IncludedEvents)
	}
}

// A body Postgres cannot parse is still stored: losing the delivery entirely is
// the one thing the inbox exists to prevent.
func TestUnparseableBodyIsStillStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

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

// This body IS valid JSON, so json.Valid alone lets it through and jsonb then
// refuses the escape, losing the delivery entirely.
func TestABodyCarryingANulEscapeIsStillStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	d := delivery("evt_1", time.Now())
	d.RawPayload = []byte(`{"note":"a\` + `u0000b"}`)
	if !json.Valid(d.RawPayload) {
		t.Fatal("the fixture body is not valid JSON, so it tests the wrong branch")
	}
	if err := f.svc.HandleDelivery(t.Context(), provider, d); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	var wrapped map[string]string
	if err := json.Unmarshal(storedDelivery(t, f, "evt_1").Payload, &wrapped); err != nil {
		t.Fatalf("stored payload is not JSON: %v", err)
	}
	if wrapped["raw_base64"] == "" {
		t.Error("a body carrying a NUL escape was stored without its bytes")
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
	svc, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       provider,
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

	provider.event = subEvent(f.orgID, "sub00000000000000031", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_old", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub00000000000000032", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_new", time.Now())); err == nil {
		t.Fatal("a second live subscription was accepted; the provider must be asked to retry")
	}

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

	provider.event = subEvent(f.orgID, "sub00000000000000031", "prod_mandate", corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_cancel", time.Now())); err != nil {
		t.Fatalf("HandleDelivery(cancel): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub00000000000000032", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_new", time.Now())); err != nil {
		t.Fatalf("the retry after the cancellation failed: %v", err)
	}
	if liveSubID(t, f) != "sub00000000000000032" {
		t.Error("the cutover did not complete")
	}
}

// Dodo's static payment links accept metadata_* query parameters, so a buyer can
// put any org id in a payload. On its own it attributes nothing.
func TestPayloadOrgIDDoesNotAttributeADelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	forged := subEvent(f.orgID, "sub_forged", "prod_mandate", corebilling.SubStatusActive)
	forged.CheckoutRef = ""
	forged.ProviderCustomerID = "cus_someone_else"
	provider.event = forged
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_forged", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if liveSubID(t, f) != "" {
		t.Error("metadata.org_id attributed a subscription")
	}
	if d := storedDelivery(t, f, "evt_forged"); !strings.HasPrefix(d.Error, "attribution") {
		t.Errorf("error = %q, want it to start with %q", d.Error, "attribution")
	}
}

// A ref nobody minted is worth no more than none at all.
func TestAnUnknownCheckoutRefDoesNotAttribute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	forged := subEvent(f.orgID, "sub_forged", "prod_mandate", corebilling.SubStatusActive)
	forged.CheckoutRef = "ref_guessed"
	forged.ProviderCustomerID = "cus_someone_else"
	provider.event = forged
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_forged", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if d := storedDelivery(t, f, "evt_forged"); !strings.HasPrefix(d.Error, "attribution") {
		t.Errorf("error = %q, want it to start with %q", d.Error, "attribution")
	}
}

// The stored ref and the sent ref are two statements, and every other attribution
// test seeds both halves itself. The session also carries the card the checkout
// was opened on, which the delivery pins.
func TestCheckoutStoresTheRefItSendsToTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	if _, _, err := f.svc.CreateCheckoutSession(t.Context(), corebilling.Checkout{
		OrgID: f.orgID, PlanSlug: corebilling.CurrentSlug, Email: "buyer@example.com", Name: "Ada Buyer",
	}); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	sent := provider.checkoutIn.CheckoutRef
	if sent == "" {
		t.Fatal("the checkout carried no ref; every delivery it produces is unattributable")
	}
	if provider.checkoutIn.ProductID != "prod_mandate" {
		t.Errorf("checkout product = %q, want the mandate product", provider.checkoutIn.ProductID)
	}

	var storedOrg, storedSlug string
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select org_id, plan_slug from billing_checkout_sessions where provider = $1 and ref = $2`,
		fakeProviderName, sent).Scan(&storedOrg, &storedSlug); err != nil {
		t.Fatalf("the ref sent to the provider was never stored: %v", err)
	}
	if storedOrg != f.orgID || storedSlug != corebilling.CurrentSlug {
		t.Errorf("ref stored against %q/%q, want %q/%q", storedOrg, storedSlug, f.orgID, corebilling.CurrentSlug)
	}

	event := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	event.CheckoutRef = sent
	event.OrgID = ""
	event.ProviderCustomerID = "cus_brand_new"
	provider.event = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != corebilling.CurrentSlug || !ent.Chargeable {
		t.Errorf("entitlement = %q chargeable=%v, want the pinned card", ent.Slug, ent.Chargeable)
	}
}

// The card is pinned at activation from the checkout's session, and a later
// delivery carrying no ref keeps the stored pin.
func TestDeliveryPinsTheCheckoutsCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	other, err := dbwriteOrg(t, f.pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	seedCheckoutRefFor(t, f, other, corebilling.SlugCustom)

	provider.event = subEvent(other, "sub_deal", "prod_mandate", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	renewal := subEvent(other, "sub_deal", "prod_mandate", corebilling.SubStatusActive)
	renewal.CheckoutRef = ""
	renewal.OrgID = ""
	provider.event = renewal
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_2", time.Now().Add(time.Minute))); err != nil {
		t.Fatalf("HandleDelivery(renewal): %v", err)
	}

	var slug string
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select plan_slug from billing_subscriptions where provider_sub_id = 'sub_deal'`).Scan(&slug); err != nil {
		t.Fatalf("read the subscription: %v", err)
	}
	if slug != corebilling.SlugCustom {
		t.Errorf("stored plan_slug = %q, want the session's custom pin kept through a ref-less renewal", slug)
	}
}

// Every other test feeds an event whose signals agree, so a reordering of the
// branches passes them all. Here they name two orgs.
func TestAttributionPrefersTheRefOverEverythingElse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	other, err := dbwriteOrg(t, f.pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	seedCheckoutRef(t, f, other)

	first := subEvent(other, "sub_other", "prod_mandate", corebilling.SubStatusActive)
	provider.event = first
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_seed", time.Now())); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	event := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	event.OrgID = other
	event.ProviderCustomerID = "cus_" + other
	provider.event = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if liveSubID(t, f) != "sub_1" {
		t.Error("attribution did not prefer the ref")
	}
}

// A payment carrying no invoice id is not pug's: the mandate's own authorization,
// say. Stored and ignored, never an error.
func TestPaymentWithoutAnInvoiceIsIgnored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.payment = corebilling.PaymentEvent{Payment: corebilling.PaymentRecord{
		PaymentID: "pay_auth", Status: corebilling.PaymentSucceeded,
	}}

	d := delivery("evt_pay", time.Now())
	d.EventType = "payment.succeeded"
	if err := f.svc.HandleDelivery(t.Context(), provider, d); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if got := storedDelivery(t, f, "evt_pay"); !got.ProcessedAt.Valid || got.Error != "" {
		t.Errorf("delivery processed=%v error=%q, want processed with no error", got.ProcessedAt.Valid, got.Error)
	}
}
