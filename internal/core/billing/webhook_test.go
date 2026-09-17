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
	// checkoutIn is the last input CreateCheckoutSession was handed, so a test can
	// join the ref pug stored against the ref it sent.
	checkoutIn corebilling.CheckoutInput
	// charges is every charge asked for; onCharge answers each, and nil succeeds.
	charges  []corebilling.ChargeInput
	onCharge func(corebilling.ChargeInput) (string, error)
	// payment is what a payment or refund delivery normalizes to.
	payment corebilling.PaymentEvent
	// payments is what the provider holds. The listing ignores since, so the settle's
	// own window is what a test holds it to, and carries no amounts; fetched records
	// every whole read.
	payments []corebilling.Payment
	listErr  error
	fetched  []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (f *fakeProvider) CanVerify() bool { return true }

func (f *fakeProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return f.event, f.err
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
	f.charges = append(f.charges, in)
	if f.onCharge != nil {
		return f.onCharge(in)
	}
	return "pay_" + in.InvoiceID, nil
}

func (f *fakeProvider) NormalizePayment(corebilling.Delivery) (corebilling.PaymentEvent, error) {
	return f.payment, nil
}

func (f *fakeProvider) ListPayments(_ context.Context, subID string, _ time.Time) ([]corebilling.Payment, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []corebilling.Payment
	for _, p := range f.payments {
		if p.ProviderSubID == subID {
			out = append(out, corebilling.Payment{
				CreatedAt: p.CreatedAt, InvoiceID: p.InvoiceID, PaymentID: p.PaymentID,
				ProviderSubID: p.ProviderSubID, Status: p.Status,
			})
		}
	}
	return out, nil
}

func (f *fakeProvider) FetchPayment(_ context.Context, id string) (corebilling.Payment, error) {
	f.fetched = append(f.fetched, id)
	for _, p := range f.payments {
		if p.PaymentID == id {
			return p, nil
		}
	}
	return corebilling.Payment{}, errors.New("fake: no such payment")
}

const (
	fakeProviderName = "fake"
	mandateProduct   = "prod_mandate"
)

func newPaidFixture(t *testing.T) (*fixture, *fakeProvider) {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	org, err := dbwriteOrg(t, pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	provider := &fakeProvider{name: fakeProviderName}
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, &corebilling.Payments{
		MandateProduct: mandateProduct,
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

// seedCheckoutRef stands in for the row CreateCheckoutSession writes. The pinned
// card rides along with it: that is what the delivery carries onto the mandate.
func seedCheckoutRef(t *testing.T, f *fixture, orgID string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_checkout_sessions (org_id, plan_slug, provider, ref) values ($1, $2, $3, $4)`,
		orgID, currentCard().Slug, fakeProviderName, checkoutRef(orgID)); err != nil {
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

// chargeable is the observable a live mandate produces now: it no longer supplies
// the plan, so the slug is the same either side of one.
func chargeable(t *testing.T, f *fixture) bool {
	t.Helper()
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	return ent.Chargeable
}

func TestDeliveryAppliesASubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if !ent.Chargeable || ent.Status != corebilling.StatusActive {
		t.Errorf("entitlement = (chargeable=%v, %s), want (true, ACTIVE)", ent.Chargeable, ent.Status)
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub_status = %q, want active", ent.SubStatus)
	}
	// The card the checkout pinned is what the mandate carries.
	if ent.Slug != currentCard().Slug {
		t.Errorf("slug = %q, want the pinned card", ent.Slug)
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

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_new", newer)); err != nil {
		t.Fatalf("HandleDelivery(newer): %v", err)
	}
	// The active delivery is genuinely older, so it must lose even though it
	// arrives second.
	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_old", older)); err != nil {
		t.Fatalf("HandleDelivery(older): %v", err)
	}

	if chargeable(t, f) {
		t.Error("a stale delivery revived a cancelled mandate")
	}
	// Accepted, not retried: the provider is not at fault for delivering in any
	// order it likes.
	if d := storedDelivery(t, f, "evt_old"); !d.ProcessedAt.Valid {
		t.Error("a stale delivery was left unprocessed, so the provider will retry it forever")
	}
}

// SubStatus.Live() is Go; the same set is hardcoded in several SQL sites with
// nothing linking them, so adding a live status in Go alone would leave an org
// uncollectable. This walks the whole vocabulary through the real query.
func TestTheLiveStatusSetAgreesBetweenGoAndSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// A slice literal `exhaustive` cannot check, so a new member must be added here
	// to be walked through SQL at all.
	if got := len(corebilling.AllSubStatuses()); got != 6 {
		t.Fatalf("vocabulary has %d statuses, want 6", got)
	}
	for _, status := range corebilling.AllSubStatuses() {
		t.Run(string(status), func(t *testing.T) {
			f, provider := newPaidFixture(t)
			provider.event = subEvent(f.orgID, "sub_1", mandateProduct, status)
			if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now().UTC())); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}

			// A live status makes the org chargeable; anything else leaves it with
			// nothing to charge.
			if got := chargeable(t, f); got != status.Live() {
				t.Errorf("status %q: chargeable = %v, but Live() = %v", status, got, status.Live())
			}
		})
	}
}

// past_due sits in 020's one-live index, not only in Live(). Every other collision
// test pairs two active rows, so dropping past_due from that predicate would let an
// org hold a second live subscription with nothing failing.
func TestPastDueHoldsTheOneLiveSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedSubscription(t, f, "sub00000000000000060", currentCard().Slug, "past_due")

	provider.event = subEvent(f.orgID, "sub00000000000000061", mandateProduct, corebilling.SubStatusActive)
	err := f.svc.HandleDelivery(t.Context(), provider, delivery("wh_past_due", time.Now()))
	if !errors.Is(err, corebilling.ErrTwoLiveSubscriptions) {
		t.Fatalf("err = %v, want ErrTwoLiveSubscriptions — past_due did not hold the slot", err)
	}
}

// A recurring subscription would be charged by the provider on its own schedule
// as well as by pug's invoices, and a tax-inclusive one would carve pug's tax out
// of pug's own amount. Both are refused where a foreign currency is.
func TestWebhookRefusesASubscriptionPugMustNotCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, mangle := range map[string]func(*corebilling.SubscriptionEvent){
		"recurring":     func(e *corebilling.SubscriptionEvent) { e.OnDemand = false },
		"tax inclusive": func(e *corebilling.SubscriptionEvent) { e.TaxInclusive = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			event := subEvent(f.orgID, "sub00000000000000031", mandateProduct, corebilling.SubStatusActive)
			mangle(&event)
			provider.event = event

			// Accepted, not retried: the provider would only send the same subscription
			// again.
			if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now().UTC())); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}
			if d := storedDelivery(t, f, "evt_1"); !d.ProcessedAt.Valid || d.Error == "" {
				t.Errorf("delivery processed=%v error=%q, want processed with a reason",
					d.ProcessedAt.Valid, d.Error)
			}
			if n := storedSubscriptions(t, f); n != 0 {
				t.Errorf("wrote %d rows for a subscription pug must not charge, want 0", n)
			}
		})
	}
}

// Nothing reads these two yet — the close that clips a last period to them lands
// later, and it cannot recover what was never stored.
func TestWebhookRecordsTheCancellationFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ended := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	event := subEvent(f.orgID, "sub00000000000000032", mandateProduct, corebilling.SubStatusCancelled)
	event.CancelAtPeriodEnd, event.EndedAt = true, ended
	provider.event = event

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now().UTC())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	var (
		onDemand, cancelAtPeriodEnd bool
		endedAt                     time.Time
	)
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select on_demand, cancel_at_period_end, ended_at from billing_subscriptions
		 where provider_sub_id = $1`, "sub00000000000000032").
		Scan(&onDemand, &cancelAtPeriodEnd, &endedAt); err != nil {
		t.Fatalf("read the stored subscription: %v", err)
	}
	if !onDemand || !cancelAtPeriodEnd || !endedAt.Equal(ended) {
		t.Errorf("stored on_demand=%v cancel_at_period_end=%v ended_at=%v, want true, true and %v",
			onDemand, cancelAtPeriodEnd, endedAt, ended)
	}
}

// A mandate resolves nothing from the org's row now, so clearing one while a
// subscription is live is an ordinary write: the org keeps its mandate and falls
// to the current card.
func TestClearUnderALiveMandateIsAllowed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ctx := t.Context()

	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, setDeal()); err != nil {
		t.Fatalf("set the deal: %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
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
	if ent.Terms != nil {
		t.Errorf("terms = %+v after the deal was cleared, want none", *ent.Terms)
	}
	if !ent.Chargeable || ent.Slug != currentCard().Slug {
		t.Errorf("entitlement = %q chargeable=%v, want the current card and the mandate kept",
			ent.Slug, ent.Chargeable)
	}
}

// A webhook's CAS stamp is the whole-second webhook-timestamp header, so a
// cutover's cancellation and activation can carry the same one. A tie must not be
// able to grant: the safe direction is withholding.
func TestASameSecondDeliveryCannotReviveACancelledSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_cancel", at)); err != nil {
		t.Fatalf("HandleDelivery(cancelled): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", at)); err != nil {
		t.Fatalf("HandleDelivery(active): %v", err)
	}

	if chargeable(t, f) {
		t.Error("a same-second delivery revived a cancelled mandate")
	}
}

// The other half of the tie-break, and the half `<` alone cannot do: a cutover's
// cancellation shares a whole second with its activation, and dropping it would
// leave the org billable on a mandate it cancelled.
func TestASameSecondCancellationEndsAnActiveSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	at := time.Now().UTC().Truncate(time.Second)

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", at)); err != nil {
		t.Fatalf("HandleDelivery(active): %v", err)
	}
	cancelled := subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusCancelled)
	cancelled.CancelAtPeriodEnd, cancelled.EndedAt = true, at
	provider.event = cancelled
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_cancel", at)); err != nil {
		t.Fatalf("HandleDelivery(cancelled): %v", err)
	}

	if chargeable(t, f) {
		t.Error("a same-second cancellation was dropped")
	}
	// The update arm rather than the insert: the activation already wrote the row.
	var (
		cancelAtPeriodEnd bool
		endedAt           time.Time
	)
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select cancel_at_period_end, ended_at from billing_subscriptions
		 where provider_sub_id = $1`, "sub_1").Scan(&cancelAtPeriodEnd, &endedAt); err != nil {
		t.Fatalf("read the stored subscription: %v", err)
	}
	if !cancelAtPeriodEnd || !endedAt.Equal(at) {
		t.Errorf("stored cancel_at_period_end=%v ended_at=%v, want true and %v",
			cancelAtPeriodEnd, endedAt, at)
	}
	// Applied, not skipped: a reason here would file it as a lost payment.
	if d := storedDelivery(t, f, "evt_cancel"); !d.ProcessedAt.Valid || d.Error != "" {
		t.Errorf("delivery processed=%v error=%q, want processed with no error",
			d.ProcessedAt.Valid, d.Error)
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

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if !chargeable(t, f) {
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

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	// A cancellation under the same webhook id: if the retry re-applied, the
	// mandate would end.
	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", at.Add(time.Hour))); err != nil {
		t.Fatalf("HandleDelivery(retry): %v", err)
	}

	if !chargeable(t, f) {
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
				e := subEvent(orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
				e.Currency = "EUR"
				return e
			},
			reason: "currency",
		},
		{
			name: "unattributable",
			event: func(string) corebilling.SubscriptionEvent {
				e := subEvent("", "sub_1", mandateProduct, corebilling.SubStatusActive)
				e.OrgID = xid.New().String()
				e.ProviderCustomerID = "cus_nobody"
				return e
			},
			reason: "attribution",
		},
		{
			name: "no status",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
				e.Status = ""
				return e
			},
			reason: "status",
		},
		{
			name: "no customer",
			event: func(orgID string) corebilling.SubscriptionEvent {
				e := subEvent(orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
				e.ProviderCustomerID = ""
				return e
			},
			reason: "customer",
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
			if got := stored.Error; !strings.HasPrefix(got, tc.reason) {
				t.Errorf("error = %q, want it to start with %q", got, tc.reason)
			}
			if chargeable(t, f) {
				t.Error("an unapplicable delivery made the org chargeable")
			}
		})
	}
}

// A product pug does not recognise is not a rejection any more: every org
// authorizes against the same one, so the product decides nothing at all.
func TestAnUnrecognisedProductStillApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = subEvent(f.orgID, "sub_1", "prod_something_else", corebilling.SubStatusActive)

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if d := storedDelivery(t, f, "evt_1"); !d.ProcessedAt.Valid || d.Error != "" {
		t.Errorf("delivery processed=%v error=%q, want processed with no error", d.ProcessedAt.Valid, d.Error)
	}
	if !chargeable(t, f) {
		t.Error("an unrecognised product withheld a mandate")
	}
}

// Attribution falls back to the provider customer when a delivery carries no
// org metadata — a renewal, say, which Dodo need not echo metadata onto. The card
// then comes from the row already stored, not from a fresh resolve.
func TestAttributionFallsBackToTheProviderCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	// Both cleared, or the ref attributes it before the customer is consulted.
	renewal := subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusPastDue)
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
	if !ent.Chargeable {
		t.Error("past_due must keep the mandate chargeable")
	}
	if ent.Slug != currentCard().Slug {
		t.Errorf("slug = %q, want the pinned card kept — a ref-less renewal re-resolved it", ent.Slug)
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
		event := subEvent(orgID, "sub_"+orgID, mandateProduct, corebilling.SubStatusActive)
		event.ProviderCustomerID = "cus_shared"
		provider.event = event
		if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_seed_"+orgID, time.Now())); err != nil {
			t.Fatalf("HandleDelivery(seed %d): %v", i, err)
		}
	}

	unattributed := subEvent("", "sub_new", mandateProduct, corebilling.SubStatusActive)
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

// The other half of storablePayload: this body IS valid JSON, so json.Valid alone
// lets it through and jsonb then refuses the escape, losing the delivery entirely.
func TestABodyCarryingANulEscapeIsStillStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = corebilling.SubscriptionEvent{}

	d := delivery("evt_1", time.Now())
	d.RawPayload = []byte(`{"note":"a\u0000b"}`)
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

// dbwriteOrg creates a backdated org, so a test asserting a deal is not also
// fighting a live trial window.
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
		MandateProduct: mandateProduct,
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

	provider.event = subEvent(f.orgID, "sub00000000000000031", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_old", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub00000000000000032", mandateProduct, corebilling.SubStatusActive)
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
	provider.event = subEvent(f.orgID, "sub00000000000000031", mandateProduct, corebilling.SubStatusCancelled)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_cancel", time.Now())); err != nil {
		t.Fatalf("HandleDelivery(cancel): %v", err)
	}
	provider.event = subEvent(f.orgID, "sub00000000000000032", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(ctx, provider, delivery("wh_new", time.Now())); err != nil {
		t.Fatalf("the retry after the cancellation failed: %v", err)
	}
	if !chargeable(t, f) {
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

	forged := subEvent(f.orgID, "sub_forged", mandateProduct, corebilling.SubStatusActive)
	forged.CheckoutRef = ""
	forged.ProviderCustomerID = "cus_someone_else"
	provider.event = forged
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_forged", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if chargeable(t, f) {
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

	forged := subEvent(f.orgID, "sub_forged", mandateProduct, corebilling.SubStatusActive)
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
// test seeds both halves itself. The ref also carries the card the buyer agreed to.
func TestCheckoutStoresTheRefAndCardItSendsToTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	card := currentCard().Slug

	if _, _, err := f.svc.CreateCheckoutSession(t.Context(), corebilling.Checkout{
		OrgID: f.orgID, PlanSlug: card, Email: "buyer@example.com", Name: "Ada Buyer",
	}); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	sent := provider.checkoutIn.CheckoutRef
	if sent == "" {
		t.Fatal("the checkout carried no ref; every delivery it produces is unattributable")
	}
	// The one product every org authorizes against, whatever slug was pinned.
	if provider.checkoutIn.ProductID != mandateProduct {
		t.Errorf("checkout product = %q, want the mandate product", provider.checkoutIn.ProductID)
	}

	var storedOrg, storedSlug string
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select org_id, plan_slug from billing_checkout_sessions where provider = $1 and ref = $2`,
		fakeProviderName, sent).Scan(&storedOrg, &storedSlug); err != nil {
		t.Fatalf("the ref sent to the provider was never stored: %v", err)
	}
	if storedOrg != f.orgID {
		t.Errorf("ref stored against %q, want %q", storedOrg, f.orgID)
	}
	if storedSlug != card {
		t.Errorf("stored card = %q, want the one the buyer was shown (%q)", storedSlug, card)
	}

	// And back: a delivery carrying only that ref lands on the org, on that card.
	event := subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
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
	if !ent.Chargeable || ent.Slug != card {
		t.Errorf("entitlement = %q chargeable=%v, want the pinned card and a live mandate",
			ent.Slug, ent.Chargeable)
	}
}

// A retired card stays pinned to the mandate that bought it: the ref carries the
// slug, so a reprice mid-flight cannot move the price the buyer agreed to.
func TestTheCheckoutsCardOutlivesARetirement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)

	// The checkout was opened on a card the catalog has since dropped.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_checkout_sessions set plan_slug = 'usage-2019-01-1' where ref = $1`,
		checkoutRef(f.orgID)); err != nil {
		t.Fatalf("repoint the checkout: %v", err)
	}

	provider.event = subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "usage-2019-01-1" {
		t.Errorf("slug = %q, want the card the checkout pinned", ent.Slug)
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

	// The customer id names `other`, via a subscription it already holds.
	provider.event = subEvent(other, "sub_other", mandateProduct, corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_seed", time.Now())); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// The ref names f.orgID; metadata.org_id and the customer both name `other`.
	event := subEvent(f.orgID, "sub_1", mandateProduct, corebilling.SubStatusActive)
	event.OrgID = other
	event.ProviderCustomerID = "cus_" + other
	provider.event = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if !chargeable(t, f) {
		t.Error("attribution did not prefer the ref")
	}
}
