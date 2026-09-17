package billing_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// removeNow is two days into the period after the fixture's periodEnd.
var removeNow = day(time.September, 14).Add(6 * time.Hour)

func pinNextCharges(t *testing.T, svc *corebilling.Service, now time.Time) corebilling.PinReport {
	t.Helper()
	r, err := svc.PinNextCharges(t.Context(), now, grace)
	if err != nil {
		t.Fatalf("PinNextCharges: %v", err)
	}
	return r
}

// scheduleCancel leaves the org's mandates as the provider's portal does: live, and
// ending at end.
func scheduleCancel(t *testing.T, f *fixture, end time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set cancel_at_period_end = true, current_period_end = $2 where org_id = $1`,
		f.orgID, end); err != nil {
		t.Fatalf("schedule a cancellation: %v", err)
	}
}

// payOnCharge answers every charge with a payment the provider then reports in status.
func payOnCharge(provider *fakeProvider, status corebilling.PaymentStatus) {
	provider.onCharge = func(in corebilling.ChargeInput) (string, error) {
		p := paymentOf("pay_"+in.InvoiceID, in.InvoiceID, in.ProviderSubID, status, removeNow)
		p.TotalCents = in.AmountCents + p.TaxCents
		provider.payments = append(provider.payments, p)
		return p.PaymentID, nil
	}
}

// removalFixture is a mandate added at periodEnd with usage every day since, metered
// through removeNow.
func removalFixture(t *testing.T) (*fixture, *fakeProvider, string) {
	t.Helper()
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, removeNow, 100_000)
	subID := seedMandate(t, f, periodEnd, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	stampMeter(t, f, removeNow)
	return f, provider, subID
}

func removePaymentMethod(t *testing.T, f *fixture) error {
	t.Helper()
	return f.svc.RemovePaymentMethod(t.Context(), f.orgID, actor, removeNow, grace)
}

// The pin sits a day past the current period's charge, is not sent again once the
// provider holds it, and moves on at the anniversary.
func TestPinFollowsTheCurrentPeriodsCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)

	if r := pinNextCharges(t, f.svc, closeNow); r != (corebilling.PinReport{Pinned: 1}) ||
		len(provider.pins) != 1 || !provider.pins[0].Equal(day(time.October, 16)) {
		t.Fatalf("report = %+v, pins = %v, want one pin at 10-16", r, provider.pins)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, closeNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if !ent.SubPeriodEnd.Equal(day(time.October, 16)) {
		t.Errorf("stored next billing date = %s, want the provider's answer to the pin", ent.SubPeriodEnd)
	}
	if r := pinNextCharges(t, f.svc, closeNow.Add(time.Hour)); r != (corebilling.PinReport{}) || len(provider.pins) != 1 {
		t.Errorf("an hour later: report = %+v, pins = %v, want nothing sent again", r, provider.pins)
	}
	if pinNextCharges(t, f.svc, day(time.October, 10)); len(provider.pins) != 2 || !provider.pins[1].Equal(day(time.November, 16)) {
		t.Errorf("pins = %v, want the next period's charge pinned at the anniversary", provider.pins)
	}
}

// A scheduled cancellation keeps its date, or it would never arrive; a pin the provider
// refuses is unreadable.
func TestPinLeavesAScheduledCancellationAndCountsARefusal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	scheduleCancel(t, f, day(time.September, 25))

	if r := pinNextCharges(t, f.svc, closeNow); r != (corebilling.PinReport{}) || len(provider.pins) != 0 {
		t.Errorf("report = %+v, pins = %v, want a scheduled cancellation left alone", r, provider.pins)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set cancel_at_period_end = false where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("resume the mandate: %v", err)
	}
	provider.pinErr = errors.New("422 Unprocessable Entity")
	if r := pinNextCharges(t, f.svc, closeNow); r != (corebilling.PinReport{Unreadable: 1}) {
		t.Errorf("report = %+v, want the refusal unreadable", r)
	}
}

// A mandate ending before its period closes has the days the meter has finalized closed
// a day ahead of the end, once, as a sweep charged at once.
func TestCloseBillsAGoingMandateBeforeItEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, day(time.September, 25), 12_500)
	seedMandate(t, f, periodEnd, time.Time{})
	scheduleCancel(t, f, day(time.September, 25))
	insertInvoice(t, f, "deferred", day(time.July, 10), day(time.July, 11), 250)

	early := day(time.September, 23).Add(18 * time.Hour)
	stampMeter(t, f, early)
	if r := closePeriods(t, f, early); r != (corebilling.CloseReport{}) {
		t.Errorf("more than a day before the end: report = %+v, want nothing closed", r)
	}

	now := day(time.September, 24).Add(6 * time.Hour)
	stampMeter(t, f, now)
	if r := closePeriods(t, f, now); r != (corebilling.CloseReport{Closed: 1, Swept: 1}) {
		t.Fatalf("report = %+v, want one close sweeping the balance", r)
	}
	got := invoices(t, f)
	if len(got) != 2 {
		t.Fatalf("invoices = %s, want the balance and one close", windows(got))
	}
	inv := got[1]
	// $4.50 in all, which a close with the mandate staying would have deferred.
	if !inv.billedFrom.Equal(periodEnd) || !inv.billedTo.Equal(day(time.September, 22)) ||
		inv.status != string(corebilling.InvoiceOpen) || inv.usage != 200 || inv.carried != 250 ||
		inv.nextAttemptAt == nil || !inv.nextAttemptAt.Equal(now) {
		t.Errorf("invoice = %+v, want [09-10, 09-22) open at once, carrying the balance", inv)
	}
	if got[0].coveredBy == nil || *got[0].coveredBy != inv.id {
		t.Errorf("balance = %+v, want it covered by the close", got[0])
	}
	if r := closePeriods(t, f, day(time.September, 25).Add(time.Hour)); r != (corebilling.CloseReport{}) {
		t.Errorf("the next day: report = %+v, want no second close", r)
	}
}

// A mandate pinned past its period's close is caught by that close, which keeps its
// notice window up to a day before the end; the next period's final days close after.
func TestCloseChargesAMandatePinnedPastItsPeriodBeforeItEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, day(time.September, 16), 100_000)
	seedMandate(t, f, periodStart, time.Time{})
	scheduleCancel(t, f, day(time.September, 16))
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	got := invoices(t, f)
	if len(got) != 1 || !got[0].billedTo.Equal(periodEnd) ||
		got[0].nextAttemptAt == nil || !got[0].nextAttemptAt.Equal(day(time.September, 15)) {
		t.Fatalf("invoices = %s, want the period closed and charged at 09-15", windows(got))
	}

	now := day(time.September, 15).Add(6 * time.Hour)
	stampMeter(t, f, now)
	closePeriods(t, f, now)
	got = invoices(t, f)
	if len(got) != 2 || !got[1].billedFrom.Equal(periodEnd) || !got[1].billedTo.Equal(day(time.September, 13)) ||
		got[1].nextAttemptAt == nil || !got[1].nextAttemptAt.Equal(now) {
		t.Errorf("invoices = %s, want [09-10, 09-13) closed before the end and charged at once", windows(got))
	}
}

// With no cancellation scheduled, or with its end unread, a close waits out its notice
// window and nothing closes ahead of the period.
func TestCloseKeepsItsNoticeWindowWithNoCancellationDated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, set := range map[string]string{
		"no cancellation": "cancel_at_period_end = false, current_period_end = '2026-09-16'",
		"an unread end":   "cancel_at_period_end = true, current_period_end = null",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			seedDaily(t, f, seedProjectFor(t, f), periodStart, day(time.September, 16), 100_000)
			seedMandate(t, f, periodStart, time.Time{})
			if _, err := f.pg.PgW.Exec(t.Context(),
				`update billing_subscriptions set `+set+` where org_id = $1`, f.orgID); err != nil {
				t.Fatalf("date the mandate: %v", err)
			}
			stampMeter(t, f, closeNow)
			closePeriods(t, f, closeNow)
			now := day(time.September, 15).Add(6 * time.Hour)
			stampMeter(t, f, now)
			closePeriods(t, f, now)

			got := invoices(t, f)
			if len(got) != 1 || got[0].nextAttemptAt == nil ||
				!got[0].nextAttemptAt.Equal(closeNow.AddDate(0, 0, corebilling.ChargeNoticeDays)) {
				t.Errorf("invoices = %s, want the period's own close, charged after its notice window", windows(got))
			}
		})
	}
}

// A cancellation landing inside an invoice's notice window brings its charge to a day
// before the end, and leaves one due sooner alone.
func TestACancellationInsideTheNoticeWindowBringsTheChargeForward(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, periodStart, time.Time{})
	waiting := seedDue(t, f, periodStart, 10_000, day(time.September, 15))
	sooner := seedDue(t, f, day(time.July, 10), 10_000, day(time.September, 13))
	scheduleCancel(t, f, day(time.September, 14).Add(12*time.Hour))

	closePeriods(t, f, closeNow)
	if state := chargeStateOf(t, f, waiting); state.nextAttemptAt == nil ||
		!state.nextAttemptAt.Equal(day(time.September, 13).Add(12*time.Hour)) {
		t.Errorf("waiting = %+v, want it charged at 09-13 12:00", state)
	}
	if state := chargeStateOf(t, f, sooner); state.nextAttemptAt == nil || !state.nextAttemptAt.Equal(day(time.September, 13)) {
		t.Errorf("sooner = %+v, want its own date kept", state)
	}
}

// Pug's own cancellation closes what the meter has finalized, charges it and the invoice
// still in its notice window at once, and cancels once both are paid.
func TestRemovePaymentMethodChargesThenCancels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, removeNow, 100_000)
	subID := seedMandate(t, f, periodStart, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	stampMeter(t, f, closeNow)
	closePeriods(t, f, closeNow)
	stampMeter(t, f, removeNow)
	payOnCharge(provider, corebilling.PaymentSucceeded)
	other := *f
	var err error
	if other.orgID, err = dbwriteOrg(t, f.pg); err != nil {
		t.Fatalf("create org: %v", err)
	}
	seedMandate(t, &other, periodStart, time.Time{})
	elsewhereDue := seedDue(t, &other, day(time.July, 10), 10_000, closeNow)
	elsewhereCharged := seedCharge(t, &other, day(time.June, 10), "sub_other", corebilling.InvoiceCharged, "pay_other", closeNow)

	if err := removePaymentMethod(t, f); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	if chargeStateOf(t, f, elsewhereDue).status != string(corebilling.InvoiceOpen) ||
		chargeStateOf(t, f, elsewhereCharged).status != string(corebilling.InvoiceCharged) || slices.Contains(provider.fetched, "pay_other") {
		t.Error("the removal charged or read another org's invoices")
	}
	got := invoices(t, f)
	if len(got) != 2 || !got[1].billedFrom.Equal(periodEnd) || !got[1].billedTo.Equal(day(time.September, 12)) ||
		got[0].status != string(corebilling.InvoicePaid) || got[1].status != string(corebilling.InvoicePaid) {
		t.Fatalf("invoices = %s, want both periods paid, the second through 09-12", windows(got))
	}
	if len(provider.charges) != 2 || !slices.Equal(provider.cancels, []string{subID}) {
		t.Errorf("charges = %d, cancels = %v, want two charges and the mandate cancelled", len(provider.charges), provider.cancels)
	}
	var actors []string
	if rows, err := f.pg.PgRO.Query(t.Context(),
		`select distinct actor from billing_invoice_events where invoice_id = $1`, got[1].id); err != nil {
		t.Fatalf("query invoice events: %v", err)
	} else {
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan actor: %v", err)
			}
			actors = append(actors, a)
		}
	}
	if !slices.Equal(actors, []string{actor}) {
		t.Errorf("actors = %v, want every move recorded as the removal's", actors)
	}
	if chargeable(t, f) {
		t.Error("still chargeable once the payment method was removed")
	}
	if err := removePaymentMethod(t, f); !errors.Is(err, corebilling.ErrNoMandate) {
		t.Errorf("a second removal = %v, want ErrNoMandate", err)
	}
}

// Anything short of a settled charge leaves the mandate live, so a decline stays
// retryable rather than written off.
func TestRemovePaymentMethodWaitsForEveryChargeToSettle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, setup := range map[string]func(*testing.T, *fixture, *fakeProvider){
		"a decline": func(_ *testing.T, _ *fixture, p *fakeProvider) { p.onCharge = declined },
		"an unanswered charge": func(_ *testing.T, _ *fixture, p *fakeProvider) {
			p.onCharge = func(corebilling.ChargeInput) (string, error) { return "", errors.New("connection reset") }
		},
		"a payment still processing": func(_ *testing.T, _ *fixture, p *fakeProvider) {
			payOnCharge(p, corebilling.PaymentProcessing)
		},
		"a meter not yet final": func(t *testing.T, f *fixture, _ *fakeProvider) { stampMeter(t, f, closeNow) },
		"a period no card prices": func(t *testing.T, f *fixture, _ *fakeProvider) {
			if _, err := f.pg.PgW.Exec(t.Context(),
				`update billing_subscriptions set plan_slug = 'retired_card' where org_id = $1`, f.orgID); err != nil {
				t.Fatalf("unknown slug: %v", err)
			}
		},
		"a retry still to come": func(t *testing.T, f *fixture, p *fakeProvider) {
			payOnCharge(p, corebilling.PaymentSucceeded)
			updateInvoice(t, f, seedDue(t, f, day(time.July, 10), 10_000, day(time.September, 20)),
				"status = 'failed', attempts = 1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider, _ := removalFixture(t)
			setup(t, f, provider)
			if err := removePaymentMethod(t, f); !errors.Is(err, corebilling.ErrFinalPeriodUnsettled) {
				t.Fatalf("RemovePaymentMethod = %v, want ErrFinalPeriodUnsettled", err)
			}
			if len(provider.cancels) != 0 || !chargeable(t, f) {
				t.Errorf("cancels = %v, want the mandate left live", provider.cancels)
			}
		})
	}
}

// A retry of the removal closes and charges nothing, still waits on the charge it made,
// and cancels once that payment succeeds.
func TestRemovePaymentMethodRetriedWaitsOnItsCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider, subID := removalFixture(t)
	payOnCharge(provider, corebilling.PaymentProcessing)
	for range 2 {
		if err := removePaymentMethod(t, f); !errors.Is(err, corebilling.ErrFinalPeriodUnsettled) {
			t.Fatalf("RemovePaymentMethod = %v, want ErrFinalPeriodUnsettled", err)
		}
	}
	if len(provider.charges) != 1 || len(invoices(t, f)) != 1 {
		t.Fatalf("charges = %d, invoices = %s, want the retry to add neither", len(provider.charges), windows(invoices(t, f)))
	}
	provider.payments[0].Status = corebilling.PaymentSucceeded
	if err := removePaymentMethod(t, f); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	if !slices.Equal(provider.cancels, []string{subID}) {
		t.Errorf("cancels = %v, want the mandate cancelled", provider.cancels)
	}
}

// The last close sweeps: a balance and a final period under the sweep's floor are
// written off rather than charged, which counts as settled.
func TestRemovePaymentMethodCancelsOverAWaivedSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, removeNow, 50_000)
	subID := seedMandate(t, f, periodEnd, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	insertInvoice(t, f, "deferred", day(time.July, 10), day(time.July, 11), 50)
	stampMeter(t, f, removeNow)

	if err := removePaymentMethod(t, f); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	got := invoices(t, f)
	if len(got) != 2 || got[0].status != string(corebilling.InvoiceWaived) || got[1].status != string(corebilling.InvoiceWaived) {
		t.Errorf("invoices = %s, want the balance and the last close waived", windows(got))
	}
	if len(provider.charges) != 0 || !slices.Equal(provider.cancels, []string{subID}) {
		t.Errorf("charges = %d, cancels = %v, want nothing charged and the mandate cancelled", len(provider.charges), provider.cancels)
	}
}

// A removal just after an anniversary closes the period whole, and that close sweeps too:
// its $2.50 is charged rather than deferred behind a mandate about to go.
func TestRemovePaymentMethodSweepsAPeriodItClosesWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, day(time.August, 11), 162_500)
	subID := seedMandate(t, f, periodStart, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	stampMeter(t, f, closeNow)
	payOnCharge(provider, corebilling.PaymentSucceeded)

	if err := f.svc.RemovePaymentMethod(t.Context(), f.orgID, actor, closeNow, grace); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	if got := invoices(t, f); len(got) != 1 || !got[0].billedTo.Equal(periodEnd) || got[0].status != string(corebilling.InvoicePaid) {
		t.Errorf("invoices = %s, want the period closed whole and paid", windows(got))
	}
}

// Inside the grace after an anniversary, the period that just ended is closed through the
// days the meter has finalized rather than left to a gone mandate.
func TestRemovePaymentMethodClosesAPeriodStillInsideItsGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, periodEnd, 100_000)
	subID := seedMandate(t, f, periodStart, time.Time{})
	provider.event = subEvent(f.orgID, subID, mandateProduct, corebilling.SubStatusActive)
	now := periodEnd.Add(30 * time.Hour)
	stampMeter(t, f, now)
	payOnCharge(provider, corebilling.PaymentSucceeded)

	if err := f.svc.RemovePaymentMethod(t.Context(), f.orgID, actor, now, grace); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	got := invoices(t, f)
	if len(got) != 1 || !got[0].billedFrom.Equal(periodStart) || !got[0].billedTo.Equal(day(time.September, 9)) ||
		got[0].status != string(corebilling.InvoicePaid) {
		t.Errorf("invoices = %s, want [08-10, 09-09) paid", windows(got))
	}
	if !slices.Equal(provider.cancels, []string{subID}) {
		t.Errorf("cancels = %v, want the mandate cancelled", provider.cancels)
	}
}

func TestRemovePaymentMethodNeedsALiveMandateAndAnActor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedMandate(t, f, periodStart, day(time.September, 1))
	if err := removePaymentMethod(t, f); !errors.Is(err, corebilling.ErrNoMandate) {
		t.Errorf("with only a cancelled mandate = %v, want ErrNoMandate", err)
	}
	if err := f.svc.RemovePaymentMethod(t.Context(), f.orgID, " ", removeNow, grace); !errors.Is(err, corebilling.ErrActorRequired) {
		t.Errorf("with no actor = %v, want ErrActorRequired", err)
	}
}
