package billing_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func declined(corebilling.ChargeInput) (string, error) {
	return "", &corebilling.ChargeError{Code: "HTTP_402", Message: "card declined", Declined: true}
}

// A soft decline is retried three days after the first attempt and seven after each
// later one, never before its date, and the fourth failure is final. The org is past
// due throughout, and still chargeable.
func TestDunningRetriesASoftDeclineThenGivesUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	provider.onCharge = declined

	at := closeNow
	for i, wait := range []time.Duration{3 * 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour} {
		if r := chargeDue(t, f.svc, at); r != (corebilling.ChargeReport{Declined: 1}) {
			t.Fatalf("attempt %d: report = %+v, want declined", i+1, r)
		}
		state := chargeStateOf(t, f, id)
		if state.status != string(corebilling.InvoiceFailed) || state.attempts != i+1 ||
			state.nextAttemptAt == nil || !state.nextAttemptAt.Equal(at.Add(wait)) {
			t.Fatalf("attempt %d: invoice = %+v, want failed and retried at %s", i+1, state, at.Add(wait))
		}
		ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, at)
		if err != nil {
			t.Fatalf("GetEntitlement: %v", err)
		}
		if ent.Status != corebilling.StatusPastDue || ent.PastDueReason != corebilling.PastDueDeclined || !ent.Chargeable {
			t.Errorf("attempt %d: entitlement = (%s, %s, chargeable=%v), want past due on a card still chargeable",
				i+1, ent.Status, ent.PastDueReason, ent.Chargeable)
		}
		if r := chargeDue(t, f.svc, at.Add(wait-time.Hour)); r != (corebilling.ChargeReport{}) {
			t.Errorf("attempt %d: report = %+v an hour before the retry, want nothing charged", i+1, r)
		}
		at = at.Add(wait)
	}
	if r := chargeDue(t, f.svc, at); r != (corebilling.ChargeReport{Uncollectible: 1}) {
		t.Errorf("fourth attempt: report = %+v, want it final", r)
	}
	if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceUncollectible) ||
		state.attempts != corebilling.MaxChargeAttempts || state.nextAttemptAt != nil {
		t.Errorf("invoice = %+v, want uncollectible after %d attempts", state, corebilling.MaxChargeAttempts)
	}
	if r := chargeDue(t, f.svc, at.AddDate(0, 1, 0)); r != (corebilling.ChargeReport{}) || len(provider.charges) != 4 {
		t.Errorf("a month later: report = %+v after %d charges, want nothing more", r, len(provider.charges))
	}
	if got := transitions(t, f, id); got[len(got)-1] != "charging>uncollectible declined HTTP_402" ||
		got[0] != "open>charging mandate "+subID {
		t.Errorf("events = %q", got)
	}
}

// The provider's six hard declines are final at once. Every other code, an unknown
// one included, is retried.
func TestDunningWritesOffAHardDeclineAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for code, hard := range map[string]bool{
		"STOLEN_CARD":            true,
		"LOST_CARD":              true,
		"PICKUP_CARD":            true,
		"DO_NOT_HONOR":           true,
		"FRAUDULENT":             true,
		"AUTHENTICATION_FAILURE": true,
		"INSUFFICIENT_FUNDS":     false,
		"A_CODE_ADDED_LATER":     false,
		"":                       false,
	} {
		t.Run(code, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, "pay_1", closeNow)
			p := paymentOf("pay_1", id, subID, corebilling.PaymentFailed, closeNow)
			p.ErrorCode = code
			provider.payments = []corebilling.Payment{p}

			want, wantStatus := corebilling.SettleReport{Failed: 1}, corebilling.InvoiceFailed
			if hard {
				want, wantStatus = corebilling.SettleReport{Uncollectible: 1}, corebilling.InvoiceUncollectible
			}
			if r := settleCharges(t, f.svc, settleNow); r != want {
				t.Errorf("report = %+v, want %+v", r, want)
			}
			state := chargeStateOf(t, f, id)
			if state.status != string(wantStatus) || state.code != code || state.attempts != 1 ||
				(state.nextAttemptAt == nil) != hard {
				t.Errorf("invoice = %+v, want %s on its one attempt", state, wantStatus)
			}
		})
	}
}

// A charge no read can find a payment for is charged again, and each lap counts, so
// the loop ends in a write-off rather than re-charging every hour. A charge the
// provider answered took nothing and spends no attempt.
func TestSettleStopsChargingWhatNoReadCanFind(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	t.Run("a charge that never lands", func(t *testing.T) {
		f, provider := newPaidFixture(t)
		seedMandate(t, f, periodStart, time.Time{})
		id := seedDue(t, f, periodStart, 10_000, closeNow)
		provider.onCharge = func(corebilling.ChargeInput) (string, error) { return "", errors.New("connection reset") }

		at := closeNow
		for lap := 1; lap <= 10; lap++ {
			chargeDue(t, f.svc, at)
			if chargeStateOf(t, f, id).status != string(corebilling.InvoiceCharging) {
				break
			}
			backdate(t, f, id, at)
			want := corebilling.SettleReport{Reopened: 1}
			if lap == corebilling.MaxChargeAttempts {
				want = corebilling.SettleReport{Uncollectible: 1}
			}
			if r := settleCharges(t, f.svc, at.Add(10*time.Minute)); r != want {
				t.Errorf("lap %d: report = %+v, want %+v", lap, r, want)
			}
			at = at.Add(time.Hour)
		}
		state := chargeStateOf(t, f, id)
		if state.status != string(corebilling.InvoiceUncollectible) || state.code != "unsettled" ||
			state.attempts != corebilling.MaxChargeAttempts || len(provider.charges) != corebilling.MaxChargeAttempts {
			t.Errorf("invoice = %+v after %d charges, want written off after %d", state, len(provider.charges),
				corebilling.MaxChargeAttempts)
		}
		if got := transitions(t, f, id); got[len(got)-1] != "charging>uncollectible no payment found" {
			t.Errorf("events = %q, want the last lap written off", got)
		}
	})

	t.Run("a refusal pug caused", func(t *testing.T) {
		f, _ := newPaidFixture(t)
		subID := seedMandate(t, f, periodStart, time.Time{})
		id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
		updateInvoice(t, f, id, "attempts = 3, last_error_code = 'HTTP_429'")

		if r := settleCharges(t, f.svc, settleNow); r != (corebilling.SettleReport{Reopened: 1}) {
			t.Errorf("report = %+v, want it reopened", r)
		}
		if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceOpen) || state.attempts != 3 {
			t.Errorf("invoice = %+v, want open with its attempts untouched", state)
		}
	})
}

// A new card gives every failed and uncollectible invoice another charge at once,
// keeping their attempts: one spent on its last attempt is final after one more. It
// does so even when the delivery is older than the subscription state stored.
func TestANewPaymentMethodReopensWhatTheOldCardCouldNotPay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub_card", mandateProduct, corebilling.SubStatusActive)
	provider.event = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_active", closeNow)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	seed := func(from time.Time, set string) string {
		id := seedDue(t, f, from, 10_000, closeNow.AddDate(0, 0, 30))
		updateInvoice(t, f, id, set)
		return id
	}
	other := *f
	var err error
	if other.orgID, err = dbwriteOrg(t, f.pg); err != nil {
		t.Fatalf("create org: %v", err)
	}
	elsewhere := seedDue(t, &other, day(time.March, 10), 10_000, closeNow)
	updateInvoice(t, f, elsewhere, "status = 'failed', attempts = 1")
	failed := seed(day(time.March, 10), "status = 'failed', attempts = 1")
	spent := seed(day(time.April, 10), "status = 'uncollectible', attempts = 4, next_attempt_at = null")
	open := seed(day(time.May, 10), "status = 'open'")
	paid := seed(day(time.June, 10), "status = 'paid', attempts = 1")
	deferred := seed(day(time.July, 10), "status = 'deferred', next_attempt_at = null")

	newCard := closeNow.Add(-time.Hour)
	event.PaymentMethodUpdated = true
	provider.event = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("evt_card", newCard)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	for id, want := range map[string]corebilling.InvoiceStatus{
		failed: corebilling.InvoiceOpen, spent: corebilling.InvoiceOpen, open: corebilling.InvoiceOpen,
		paid: corebilling.InvoicePaid, deferred: corebilling.InvoiceDeferred, elsewhere: corebilling.InvoiceFailed,
	} {
		state := chargeStateOf(t, f, id)
		reopened := id == failed || id == spent
		if state.status != string(want) || reopened != (state.nextAttemptAt != nil && state.nextAttemptAt.Equal(newCard)) {
			t.Errorf("invoice %s = %+v, want %s, due at the new card only if reopened", id, state, want)
		}
	}
	if got, want := transitions(t, f, spent), []string{"uncollectible>open payment method updated"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}

	provider.onCharge = declined
	if r := chargeDue(t, f.svc, newCard); r != (corebilling.ChargeReport{Declined: 1, Uncollectible: 1}) {
		t.Errorf("report = %+v, want the invoice with attempts left retried and the spent one final", r)
	}
	if state := chargeStateOf(t, f, failed); state.status != string(corebilling.InvoiceFailed) || state.attempts != 2 ||
		state.nextAttemptAt == nil || !state.nextAttemptAt.Equal(newCard.AddDate(0, 0, 7)) {
		t.Errorf("invoice = %+v, want its second attempt retried in seven days", state)
	}
}

// PAST_DUE is read off the ledger on every load: an invoice that failed and is not yet
// paid, a retry in flight included, never a deferred one, and nothing with billing off.
// Its reason is what fixes it: a card update while a mandate is live, else a checkout.
func TestPastDueIsReadOffTheLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pastDue := func(t *testing.T, f *fixture) corebilling.Entitlement {
		t.Helper()
		ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, closeNow)
		if err != nil {
			t.Fatalf("GetEntitlement: %v", err)
		}
		return ent
	}
	for _, tc := range []struct {
		set     string
		pastDue bool
	}{
		{"status = 'failed'", true}, {"status = 'uncollectible'", true},
		{"status = 'open', failed_at = now()", true}, {"status = 'charging', failed_at = now()", true},
		{"status = 'charged', failed_at = now()", true},
		{"status = 'open'", false}, {"status = 'charging'", false}, {"status = 'charged'", false},
		{"status = 'paid', failed_at = now()", false}, {"status = 'deferred'", false},
		{"status = 'waived'", false}, {"status = 'void'", false}, {"status = 'refunded'", false},
	} {
		t.Run(tc.set, func(t *testing.T) {
			f := newFixture(t)
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			updateInvoice(t, f, id, tc.set)

			if ent := pastDue(t, f); (ent.Status == corebilling.StatusPastDue) != tc.pastDue ||
				(ent.PastDueReason == corebilling.PastDueReauthorize) != tc.pastDue {
				t.Errorf("entitlement = (%s, %q), want past due = %v and a checkout to fix it",
					ent.Status, ent.PastDueReason, tc.pastDue)
			}
		})
	}

	t.Run("a live mandate", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, periodStart, time.Time{})
		updateInvoice(t, f, seedDue(t, f, periodStart, 10_000, closeNow), "status = 'failed'")
		if ent := pastDue(t, f); ent.Status != corebilling.StatusPastDue || ent.PastDueReason != corebilling.PastDueDeclined {
			t.Errorf("entitlement = (%s, %q), want past due with a card to update", ent.Status, ent.PastDueReason)
		}
	})

	t.Run("another org's invoice", func(t *testing.T) {
		f := newFixture(t)
		other := *f
		var err error
		if other.orgID, err = dbwriteOrg(t, f.pg); err != nil {
			t.Fatalf("create org: %v", err)
		}
		updateInvoice(t, f, seedDue(t, &other, periodStart, 10_000, closeNow), "status = 'failed'")
		if ent := pastDue(t, f); ent.Status == corebilling.StatusPastDue {
			t.Errorf("entitlement = (%s, %q), want nothing owed by this org", ent.Status, ent.PastDueReason)
		}
	})

	t.Run("billing off", func(t *testing.T) {
		f := newFixture(t)
		id := seedDue(t, f, periodStart, 10_000, closeNow)
		updateInvoice(t, f, id, "status = 'failed'")
		svc, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, nil)
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		ent, err := svc.GetEntitlement(t.Context(), f.orgID, closeNow)
		if err != nil {
			t.Fatalf("GetEntitlement: %v", err)
		}
		if ent.Status != corebilling.StatusFree || ent.PastDueReason != "" {
			t.Errorf("entitlement = (%s, %q), want free with no reason", ent.Status, ent.PastDueReason)
		}
	})
}

// A charge the provider accepted and then failed is dunned on the same schedule. Its
// attempt was counted when it was accepted, so the decline does not count it again.
func TestDunningDatesADeclineAfterTheChargeWasAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, tc := range []struct {
		attempts int
		want     corebilling.InvoiceStatus
		retryIn  int
	}{
		{1, corebilling.InvoiceFailed, 3}, {3, corebilling.InvoiceFailed, 7}, {4, corebilling.InvoiceUncollectible, 0},
	} {
		t.Run(fmt.Sprintf("attempt %d", tc.attempts), func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, "pay_1", closeNow)
			updateInvoice(t, f, id, fmt.Sprintf("attempts = %d", tc.attempts))
			provider.payments = []corebilling.Payment{paymentOf("pay_1", id, subID, corebilling.PaymentFailed, closeNow)}

			settleCharges(t, f.svc, settleNow)
			state := chargeStateOf(t, f, id)
			if state.status != string(tc.want) || state.attempts != tc.attempts ||
				(tc.retryIn == 0) != (state.nextAttemptAt == nil) ||
				tc.retryIn > 0 && !state.nextAttemptAt.Equal(settleNow.AddDate(0, 0, tc.retryIn)) {
				t.Errorf("invoice = %+v, want %s on %d attempts, retried in %d days", state, tc.want, tc.attempts, tc.retryIn)
			}
		})
	}
}

// A checkout is a new payment method too: a mandate going live reopens what failed on
// another mandate or on none, but never its own declines, so its renewals cannot cut
// its retries short.
func TestANewMandateReopensWhatItHasNotTried(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	other := *f
	var err error
	if other.orgID, err = dbwriteOrg(t, f.pg); err != nil {
		t.Fatalf("create org: %v", err)
	}
	seed := func(f *fixture, from time.Time, set string) string {
		id := seedDue(t, f, from, 10_000, closeNow)
		updateInvoice(t, f, id, "failed_at = now(), "+set)
		return id
	}
	gone := seed(f, day(time.March, 10), "status = 'uncollectible', next_attempt_at = null, provider_sub_id = 'sub_old'")
	neverTried := seed(f, day(time.April, 10), "status = 'uncollectible', next_attempt_at = null")
	own := seed(f, day(time.May, 10), "status = 'failed', attempts = 1, provider_sub_id = 'sub_new'")
	elsewhere := seed(&other, day(time.March, 10), "status = 'uncollectible', next_attempt_at = null")

	provider.event = subEvent(f.orgID, "sub_new", mandateProduct, corebilling.SubStatusActive)
	for i, id := range []string{"evt_active", "evt_renewed"} {
		if err := f.svc.HandleDelivery(t.Context(), provider, delivery(id, closeNow.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatalf("HandleDelivery %s: %v", id, err)
		}
	}
	for id, want := range map[string]corebilling.InvoiceStatus{
		gone: corebilling.InvoiceOpen, neverTried: corebilling.InvoiceOpen,
		own: corebilling.InvoiceFailed, elsewhere: corebilling.InvoiceUncollectible,
	} {
		if state := chargeStateOf(t, f, id); state.status != string(want) {
			t.Errorf("invoice %s = %+v, want %s", id, state, want)
		}
	}
	if got, want := transitions(t, f, gone), []string{"uncollectible>open new mandate sub_new"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, closeNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusPastDue || ent.PastDueReason != corebilling.PastDueDeclined {
		t.Errorf("entitlement = (%s, %q), want past due on a live mandate until a retry is paid",
			ent.Status, ent.PastDueReason)
	}
}
