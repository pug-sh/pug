package billing_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/rs/xid"
)

// settleNow is past both waits for a charge entered at closeNow.
var settleNow = closeNow.Add(2 * time.Hour)

func settleCharges(t *testing.T, svc *corebilling.Service, now time.Time) corebilling.SettleReport {
	t.Helper()
	r, err := svc.SettleCharges(t.Context(), now)
	if err != nil {
		t.Fatalf("SettleCharges: %v", err)
	}
	return r
}

// recordEvent appends a transition as its writer would have recorded it at `at`.
func recordEvent(t *testing.T, f *fixture, id string, from, to corebilling.InvoiceStatus, detail string, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_invoice_events (actor, at, detail, from_status, id, invoice_id, to_status)
		 values ('test', $1, $2, $3, $4, $5, $6)`, at, detail, from, xid.New().String(), id, to); err != nil {
		t.Fatalf("record invoice event: %v", err)
	}
}

// seedCharge stores a $100 invoice a charge on subID moved into status at `at`:
// charging with no answer yet, or charged on paymentID.
func seedCharge(
	t *testing.T, f *fixture, from time.Time, subID string, status corebilling.InvoiceStatus, paymentID string, at time.Time,
) string {
	t.Helper()
	id := seedDue(t, f, from, 10_000, at)
	updateInvoice(t, f, id, fmt.Sprintf("status = 'charging', provider = 'fake', provider_sub_id = '%s'", subID))
	recordEvent(t, f, id, corebilling.InvoiceOpen, corebilling.InvoiceCharging, "mandate "+subID, at)
	if status == corebilling.InvoiceCharged {
		updateInvoice(t, f, id, fmt.Sprintf("status = 'charged', attempts = 1, provider_payment_id = '%s'", paymentID))
		recordEvent(t, f, id, corebilling.InvoiceCharging, corebilling.InvoiceCharged, "payment "+paymentID, at)
	}
	return id
}

// backdate moves when the invoice entered its current status to `at`, as an earlier
// pass would have recorded it.
func backdate(t *testing.T, f *fixture, id string, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_invoice_events e set at = $2 from billing_invoices i
		 where i.id = $1 and e.invoice_id = i.id and e.to_status = i.status and e.from_status <> e.to_status`,
		id, at); err != nil {
		t.Fatalf("backdate invoice: %v", err)
	}
}

// paymentOf is a payment the provider holds for invoiceID on subID: $100 billed,
// with $18 of tax on top.
func paymentOf(id, invoiceID, subID string, status corebilling.PaymentStatus, at time.Time) corebilling.Payment {
	p := corebilling.Payment{
		CreatedAt: at, Currency: "USD", InvoiceID: invoiceID, InvoiceURL: "https://pay.example/receipt/" + id,
		PaymentID: id, ProviderSubID: subID, Status: status, TaxCents: 1_800, TotalCents: 11_800,
	}
	if status == corebilling.PaymentFailed {
		p.ErrorCode, p.ErrorMessage = "INSUFFICIENT_FUNDS", "insufficient funds"
	}
	return p
}

type paidState struct {
	paidAt   *time.Time
	taxCents *int64
	url      string
}

func paidStateOf(t *testing.T, f *fixture, id string) paidState {
	t.Helper()
	var s paidState
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select paid_at, tax_cents, coalesce(provider_invoice_url, '') from billing_invoices where id = $1`,
		id).Scan(&s.paidAt, &s.taxCents, &s.url); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return s
}

// A charge whose answer never came is settled off its mandate's payments since the
// claim: the newest that has not failed is adopted, a lone failure is a decline,
// and no payment at all is charged again.
func TestSettleReadsAnUnansweredChargeOffItsMandatesPayments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	type payments func(invoiceID, subID string) []corebilling.Payment
	one := func(status corebilling.PaymentStatus, at time.Time) payments {
		return func(invoiceID, subID string) []corebilling.Payment {
			return []corebilling.Payment{paymentOf("pay_1", invoiceID, subID, status, at)}
		}
	}
	for name, tc := range map[string]struct {
		code       string
		payments   payments
		want       corebilling.SettleReport
		wantStatus corebilling.InvoiceStatus
		wantPay    string
		attempts   int
		events     []string
	}{
		"a payment still processing is adopted": {
			payments: one(corebilling.PaymentProcessing, closeNow),
			want:     corebilling.SettleReport{Adopted: 1}, wantStatus: corebilling.InvoiceCharged, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>charged payment pay_1"},
		},
		"a payment that succeeded is paid": {
			payments: one(corebilling.PaymentSucceeded, closeNow),
			want:     corebilling.SettleReport{Paid: 1}, wantStatus: corebilling.InvoicePaid, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>paid payment pay_1"},
		},
		"a lone failed payment is a decline": {
			payments: one(corebilling.PaymentFailed, closeNow),
			want:     corebilling.SettleReport{Failed: 1}, wantStatus: corebilling.InvoiceFailed, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>failed declined INSUFFICIENT_FUNDS"},
		},
		"no payment is charged again": {
			want:       corebilling.SettleReport{Reopened: 1},
			wantStatus: corebilling.InvoiceOpen, attempts: 1, events: []string{"charging>open no payment found"},
		},
		"a refusal pug caused spends no attempt": {
			code: "HTTP_429",
			want: corebilling.SettleReport{Reopened: 1}, wantStatus: corebilling.InvoiceOpen,
			events: []string{"charging>open no payment found"},
		},
		"an earlier attempt's payment is not this one's": {
			payments: one(corebilling.PaymentSucceeded, closeNow.Add(-2*time.Minute)),
			want:     corebilling.SettleReport{Reopened: 1}, wantStatus: corebilling.InvoiceOpen, attempts: 1,
			events: []string{"charging>open no payment found"},
		},
		"a payment inside the clock skew is this one's": {
			payments: one(corebilling.PaymentSucceeded, closeNow.Add(-30*time.Second)),
			want:     corebilling.SettleReport{Paid: 1}, wantStatus: corebilling.InvoicePaid, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>paid payment pay_1"},
		},
		"an undated payment is this one's": {
			payments: one(corebilling.PaymentProcessing, time.Time{}),
			want:     corebilling.SettleReport{Adopted: 1}, wantStatus: corebilling.InvoiceCharged, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>charged payment pay_1"},
		},
		"another invoice's payment is not this one's": {
			payments: func(_, subID string) []corebilling.Payment {
				return []corebilling.Payment{paymentOf("pay_1", "other", subID, corebilling.PaymentSucceeded, closeNow)}
			},
			want: corebilling.SettleReport{Reopened: 1}, wantStatus: corebilling.InvoiceOpen, attempts: 1,
			events: []string{"charging>open no payment found"},
		},
		"a payment that has not failed wins over a newer failure": {
			payments: func(invoiceID, subID string) []corebilling.Payment {
				return []corebilling.Payment{
					paymentOf("pay_2", invoiceID, subID, corebilling.PaymentFailed, closeNow.Add(2*time.Second)),
					paymentOf("pay_1", invoiceID, subID, corebilling.PaymentProcessing, closeNow.Add(time.Second)),
				}
			},
			want: corebilling.SettleReport{Adopted: 1}, wantStatus: corebilling.InvoiceCharged, wantPay: "pay_1",
			attempts: 1, events: []string{"charging>charged payment pay_1"},
		},
		"two payments that have not failed are reported": {
			payments: func(invoiceID, subID string) []corebilling.Payment {
				return []corebilling.Payment{
					paymentOf("pay_1", invoiceID, subID, corebilling.PaymentSucceeded, closeNow.Add(time.Second)),
					paymentOf("pay_2", invoiceID, subID, corebilling.PaymentProcessing, closeNow.Add(2*time.Second)),
				}
			},
			want: corebilling.SettleReport{Adopted: 1, Duplicate: 1}, wantStatus: corebilling.InvoiceCharged,
			wantPay: "pay_2", attempts: 1,
			events: []string{"charging>charging duplicate payment pay_1", "charging>charged payment pay_2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
			if tc.code != "" {
				updateInvoice(t, f, id, "last_error_code = '"+tc.code+"'")
			}
			if tc.payments != nil {
				provider.payments = tc.payments(id, subID)
			}

			if r := settleCharges(t, f.svc, settleNow); r != tc.want {
				t.Errorf("report = %+v, want %+v", r, tc.want)
			}
			state := chargeStateOf(t, f, id)
			if state.status != string(tc.wantStatus) || state.paymentID != tc.wantPay || state.attempts != tc.attempts {
				t.Errorf("invoice = %+v, want %s on %q with %d attempts", state, tc.wantStatus, tc.wantPay, tc.attempts)
			}
			if got := transitions(t, f, id)[1:]; !slices.Equal(got, tc.events) {
				t.Errorf("events = %q, want %q", got, tc.events)
			}
			switch tc.wantStatus {
			case corebilling.InvoiceOpen:
				if state.nextAttemptAt == nil || !state.nextAttemptAt.Equal(settleNow) {
					t.Errorf("next_attempt_at = %v, want %s", state.nextAttemptAt, settleNow)
				}
			case corebilling.InvoiceFailed:
				if state.code != "INSUFFICIENT_FUNDS" || state.message != "insufficient funds" || state.nextAttemptAt != nil {
					t.Errorf("invoice = %+v, want the payment's decline with no retry dated", state)
				}
			case corebilling.InvoicePaid:
				// The list carries no amounts, so the tax could only come from a whole read.
				if paid := paidStateOf(t, f, id); paid.taxCents == nil || *paid.taxCents != 1_800 ||
					paid.paidAt == nil || !paid.paidAt.Equal(settleNow) || paid.url == "" {
					t.Errorf("paid = %+v, want the tax, the settle time and the receipt", paid)
				}
			case corebilling.InvoiceWaived, corebilling.InvoiceDeferred, corebilling.InvoiceCharging,
				corebilling.InvoiceCharged, corebilling.InvoiceRefunded, corebilling.InvoiceUncollectible:
			}
		})
	}
}

// The charge that timed out after the provider took it is settled by reading and
// never charged twice.
func TestSettleNeverChargesAPaymentTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	provider.onCharge = func(in corebilling.ChargeInput) (string, error) {
		provider.payments = append(provider.payments,
			paymentOf("pay_1", in.InvoiceID, in.ProviderSubID, corebilling.PaymentProcessing, closeNow))
		return "", errors.New("dodo: charge subscription: context deadline exceeded")
	}
	chargeDue(t, f.svc, closeNow)
	backdate(t, f, id, closeNow)

	if r := settleCharges(t, f.svc, closeNow.Add(10*time.Minute)); r != (corebilling.SettleReport{Adopted: 1}) {
		t.Fatalf("report = %+v, want the payment adopted", r)
	}
	chargeDue(t, f.svc, closeNow.Add(time.Hour))
	provider.payments[0].Status = corebilling.PaymentSucceeded
	backdate(t, f, id, closeNow)
	if r := settleCharges(t, f.svc, settleNow); r != (corebilling.SettleReport{Paid: 1}) {
		t.Errorf("report = %+v, want it paid once polled", r)
	}

	if len(provider.charges) != 1 {
		t.Errorf("charges = %d, want 1", len(provider.charges))
	}
	want := []string{"open>charging mandate " + subID, "charging>charged payment pay_1", "charged>paid payment pay_1"}
	if got := transitions(t, f, id); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// A charge that never reached the provider is charged once more after a read finds
// no payment. Only a charge the provider answered keeps its attempt.
func TestSettleChargesAgainWhatNeverArrived(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		err      error
		attempts int
	}{
		"no answer":            {errors.New("connection reset"), 2},
		"a refusal pug caused": {&corebilling.ChargeError{Code: "HTTP_429", Message: "slow down"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			seedMandate(t, f, periodStart, time.Time{})
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			provider.onCharge = func(corebilling.ChargeInput) (string, error) { return "", tc.err }
			chargeDue(t, f.svc, closeNow)
			backdate(t, f, id, closeNow)

			if r := settleCharges(t, f.svc, closeNow.Add(10*time.Minute)); r != (corebilling.SettleReport{Reopened: 1}) {
				t.Fatalf("report = %+v, want it reopened", r)
			}
			provider.onCharge = nil
			if r := chargeDue(t, f.svc, closeNow.Add(time.Hour)); r != (corebilling.ChargeReport{Charged: 1}) {
				t.Errorf("report = %+v, want it charged again", r)
			}
			if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceCharged) ||
				state.attempts != tc.attempts || len(provider.charges) != 2 {
				t.Errorf("invoice = %+v after %d charges, want charged twice with %d attempts",
					state, len(provider.charges), tc.attempts)
			}
		})
	}
}

// A read waits: a charging invoice a few minutes for its payment to list, a charged
// one an hour for its webhook, each dated from the move and not from its last update.
func TestSettleWaitsBeforeReading(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	charging := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
	charged := seedCharge(t, f, day(time.July, 10), subID, corebilling.InvoiceCharged, "pay_1", closeNow)
	provider.payments = []corebilling.Payment{paymentOf("pay_1", charged, subID, corebilling.PaymentSucceeded, closeNow)}
	// As a recorded charge error does, which moves update_time and not the claim.
	updateInvoice(t, f, charging, "last_error_message = 'timeout'")
	// A finding recorded beside a charge moves nothing either.
	recordEvent(t, f, charged, corebilling.InvoiceCharged, corebilling.InvoiceCharged,
		"partial refund rf_1 of $1.00", closeNow.Add(59*time.Minute))

	if r := settleCharges(t, f.svc, closeNow.Add(4*time.Minute)); r != (corebilling.SettleReport{}) {
		t.Errorf("four minutes in: report = %+v, want nothing read", r)
	}
	if r := settleCharges(t, f.svc, closeNow.Add(6*time.Minute)); r != (corebilling.SettleReport{Reopened: 1}) {
		t.Errorf("six minutes in: report = %+v, want only the charging invoice settled", r)
	}
	if r := settleCharges(t, f.svc, closeNow.Add(61*time.Minute)); r != (corebilling.SettleReport{Paid: 1}) {
		t.Errorf("an hour in: report = %+v, want the charged invoice polled", r)
	}
}

// A charge the provider accepted is settled off its own payment. Its attempt was
// counted when it was charged, so nothing counts it again.
func TestSettlePollsAnAcceptedCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		status     corebilling.PaymentStatus
		want       corebilling.SettleReport
		wantStatus corebilling.InvoiceStatus
	}{
		"succeeded":        {corebilling.PaymentSucceeded, corebilling.SettleReport{Paid: 1}, corebilling.InvoicePaid},
		"failed":           {corebilling.PaymentFailed, corebilling.SettleReport{Failed: 1}, corebilling.InvoiceFailed},
		"still processing": {corebilling.PaymentProcessing, corebilling.SettleReport{Pending: 1}, corebilling.InvoiceCharged},
		"unknown":          {"", corebilling.SettleReport{Unreadable: 1}, corebilling.InvoiceCharged},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, "pay_1", closeNow)
			if tc.status != "" {
				provider.payments = []corebilling.Payment{paymentOf("pay_1", id, subID, tc.status, closeNow)}
			}

			if r := settleCharges(t, f.svc, settleNow); r != tc.want {
				t.Errorf("report = %+v, want %+v", r, tc.want)
			}
			if state := chargeStateOf(t, f, id); state.status != string(tc.wantStatus) || state.attempts != 1 ||
				state.paymentID != "pay_1" {
				t.Errorf("invoice = %+v, want %s on pay_1 with its one attempt", state, tc.wantStatus)
			}
		})
	}
}

// Deferred rows are paid with the invoice that carries them.
func TestSettlePaysTheRowsACarrierCovers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	carrier := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, "pay_1", closeNow)
	provider.payments = []corebilling.Payment{paymentOf("pay_1", carrier, subID, corebilling.PaymentSucceeded, closeNow)}
	var covered []string
	for i, coveredBy := range []any{carrier, carrier, nil} {
		id := xid.New().String()
		from := day(time.June, 10).AddDate(0, 0, 10*i)
		if _, err := f.pg.PgW.Exec(t.Context(),
			`insert into billing_invoices (
			   amount_cents, billed_from, billed_to, covered_by, currency, event_count, id, lines, org_id,
			   period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
			 values (100, $1, $2, $3, 'USD', 0, $4, '[]', $5, $6, $7, 'x', '{}', 'deferred', 100, now())`,
			from, from.AddDate(0, 0, 10), coveredBy, id, f.orgID, from.AddDate(0, 1, 0), from); err != nil {
			t.Fatalf("seed a deferred invoice: %v", err)
		}
		covered = append(covered, id)
	}

	settleCharges(t, f.svc, settleNow)
	for i, id := range covered {
		state, paid := chargeStateOf(t, f, id), paidStateOf(t, f, id)
		if i == 2 {
			if state.status != string(corebilling.InvoiceDeferred) || paid.paidAt != nil {
				t.Errorf("uncovered row = %+v, want it still deferred", state)
			}
			continue
		}
		if state.status != string(corebilling.InvoicePaid) || paid.paidAt == nil || !paid.paidAt.Equal(settleNow) {
			t.Errorf("covered row = %+v %+v, want paid with its carrier", state, paid)
		}
		if got, want := transitions(t, f, id), []string{"deferred>paid covered by " + carrier}; !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	}
}

// A payment that took the wrong amount still pays the invoice, since the money
// moved, but the invoice says so. The tax Dodo added on top is not a mismatch.
func TestSettleChecksWhatAPaymentTookBeforeTax(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		mangle    func(*corebilling.Payment)
		wantEvent string
	}{
		"the billed amount with tax on top": {mangle: func(*corebilling.Payment) {}},
		"a different amount": {
			mangle:    func(p *corebilling.Payment) { p.TotalCents += 200 },
			wantEvent: "paid>paid amount_mismatch: payment pay_1 took $102.00 USD before tax, billed $100.00 USD",
		},
		"another currency": {
			mangle:    func(p *corebilling.Payment) { p.Currency = "eur" },
			wantEvent: "paid>paid amount_mismatch: payment pay_1 took $100.00 EUR before tax, billed $100.00 USD",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, "pay_1", closeNow)
			p := paymentOf("pay_1", id, subID, corebilling.PaymentSucceeded, closeNow)
			tc.mangle(&p)
			provider.payments = []corebilling.Payment{p}

			want := corebilling.SettleReport{Paid: 1}
			if tc.wantEvent != "" {
				want.AmountMismatch = 1
			}
			if r := settleCharges(t, f.svc, settleNow); r != want {
				t.Errorf("report = %+v, want %+v", r, want)
			}
			wantEvents := []string{"charged>paid payment pay_1"}
			if tc.wantEvent != "" {
				wantEvents = append(wantEvents, tc.wantEvent)
			}
			if got := transitions(t, f, id)[2:]; !slices.Equal(got, wantEvents) {
				t.Errorf("events = %q, want %q", got, wantEvents)
			}
			if paid := paidStateOf(t, f, id); paid.taxCents == nil || *paid.taxCents != 1_800 {
				t.Errorf("tax_cents = %v, want 1800", paid.taxCents)
			}
		})
	}
}

// deliverPayment runs a payment or refund delivery through the inbox.
func deliverPayment(t *testing.T, f *fixture, provider *fakeProvider, id string, event corebilling.PaymentEvent) {
	t.Helper()
	provider.payment = event
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery(id, settleNow)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
}

// A payment delivery settles the invoice its metadata names, and only from the
// mandate that invoice was charged on: a payment link lets a buyer set the id.
func TestPaymentDeliverySettlesTheInvoiceItNames(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	type setup func(t *testing.T, f *fixture, subID string) string
	charged := func(paymentID string) setup {
		return func(t *testing.T, f *fixture, subID string) string {
			return seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, paymentID, closeNow)
		}
	}
	movedTo := func(status corebilling.InvoiceStatus) setup {
		return func(t *testing.T, f *fixture, subID string) string {
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
			updateInvoice(t, f, id, "status = '"+string(status)+"'")
			return id
		}
	}
	for name, tc := range map[string]struct {
		setup      setup
		mangle     func(p *corebilling.Payment, subID string)
		status     corebilling.PaymentStatus
		wantStatus corebilling.InvoiceStatus
		rejected   bool
	}{
		"a success on a charged invoice": {
			setup: charged("pay_1"), status: corebilling.PaymentSucceeded, wantStatus: corebilling.InvoicePaid,
		},
		"a success on a charge settle reopened": {
			setup: movedTo(corebilling.InvoiceOpen), status: corebilling.PaymentSucceeded, wantStatus: corebilling.InvoicePaid,
		},
		"a success on an uncollectible invoice": {
			setup: movedTo(corebilling.InvoiceUncollectible), status: corebilling.PaymentSucceeded,
			wantStatus: corebilling.InvoicePaid,
		},
		"the failure of the payment the invoice holds": {
			setup: charged("pay_1"), status: corebilling.PaymentFailed, wantStatus: corebilling.InvoiceFailed,
		},
		"the failure of an earlier attempt": {
			setup: charged("pay_2"), status: corebilling.PaymentFailed, wantStatus: corebilling.InvoiceCharged,
		},
		"a failure while a charge is unanswered": {
			setup: movedTo(corebilling.InvoiceCharging), status: corebilling.PaymentFailed,
			wantStatus: corebilling.InvoiceCharging,
		},
		"a payment naming no invoice": {
			setup: charged("pay_1"), status: corebilling.PaymentSucceeded, wantStatus: corebilling.InvoiceCharged,
			mangle: func(p *corebilling.Payment, _ string) { p.InvoiceID = "" },
		},
		"a payment naming an invoice pug does not have": {
			setup: charged("pay_1"), status: corebilling.PaymentSucceeded, wantStatus: corebilling.InvoiceCharged,
			mangle: func(p *corebilling.Payment, _ string) { p.InvoiceID = xid.New().String() }, rejected: true,
		},
		"another mandate's payment": {
			setup: charged("pay_1"), status: corebilling.PaymentSucceeded, wantStatus: corebilling.InvoiceCharged,
			mangle: func(p *corebilling.Payment, _ string) { p.ProviderSubID = "sub_link" }, rejected: true,
		},
		"a payment for an invoice never charged": {
			setup: func(t *testing.T, f *fixture, _ string) string {
				return seedDue(t, f, periodStart, 10_000, closeNow)
			},
			mangle:     func(p *corebilling.Payment, _ string) { p.ProviderSubID = "" },
			status:     corebilling.PaymentSucceeded,
			wantStatus: corebilling.InvoiceOpen, rejected: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := tc.setup(t, f, subID)
			p := paymentOf("pay_1", id, subID, tc.status, settleNow)
			if tc.mangle != nil {
				tc.mangle(&p, subID)
			}

			deliverPayment(t, f, provider, "evt_1", corebilling.PaymentEvent{Payment: p})
			if state := chargeStateOf(t, f, id); state.status != string(tc.wantStatus) {
				t.Errorf("invoice = %+v, want %s", state, tc.wantStatus)
			}
			if d := storedDelivery(t, f, "evt_1"); !d.ProcessedAt.Valid || (d.Error != "") != tc.rejected {
				t.Errorf("delivery processed=%v error=%q, want processed and rejected=%v",
					d.ProcessedAt.Valid, d.Error, tc.rejected)
			}
			if tc.wantStatus == corebilling.InvoicePaid {
				var actor string
				if err := f.pg.PgRO.QueryRow(t.Context(),
					`select actor from billing_invoice_events where invoice_id = $1 and to_status = 'paid'`,
					id).Scan(&actor); err != nil || actor != "webhook evt_1" {
					t.Errorf("paid by %q (%v), want the delivery named", actor, err)
				}
				if r := chargeDue(t, f.svc, settleNow.AddDate(0, 0, 1)); r != (corebilling.ChargeReport{}) {
					t.Errorf("charge report = %+v, want a paid invoice left alone", r)
				}
			}
		})
	}
}

// A success that cannot land took money with no bill behind it, and the invoice
// keeps it for a refund by hand. The same payment seen twice is not one.
func TestASuccessThatCannotLandIsRecordedAsADuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		status    corebilling.InvoiceStatus
		held      string
		wantEvent string
	}{
		"on a void invoice": {
			status: "void", wantEvent: "void>void duplicate payment pay_1",
		},
		"a second payment on a paid invoice": {
			status: corebilling.InvoicePaid, held: "pay_0", wantEvent: "paid>paid duplicate payment pay_1",
		},
		"the payment a paid invoice already holds": {
			status: corebilling.InvoicePaid, held: "pay_1",
		},
		"the payment a refunded invoice already holds": {
			status: corebilling.InvoiceRefunded, held: "pay_1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
			set := "status = '" + string(tc.status) + "'"
			if tc.held != "" {
				set += ", provider_payment_id = '" + tc.held + "'"
			}
			updateInvoice(t, f, id, set)

			deliverPayment(t, f, provider, "evt_1", corebilling.PaymentEvent{
				Payment: paymentOf("pay_1", id, subID, corebilling.PaymentSucceeded, settleNow),
			})
			var want []string
			if tc.wantEvent != "" {
				want = []string{tc.wantEvent}
			}
			if got := transitions(t, f, id)[1:]; !slices.Equal(got, want) {
				t.Errorf("events = %q, want %q", got, want)
			}
			if state := chargeStateOf(t, f, id); state.status != string(tc.status) || state.paymentID != tc.held {
				t.Errorf("invoice = %+v, want left %s on %q", state, tc.status, tc.held)
			}
		})
	}
}

// Only a full refund of a paid invoice moves it; a partial one is recorded beside it.
func TestRefundDeliveryRecordsMoneyReturned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		status     corebilling.InvoiceStatus
		paymentID  string
		partial    bool
		wantStatus corebilling.InvoiceStatus
		wantEvents []string
		rejected   bool
	}{
		"a full refund of a paid invoice": {
			status: corebilling.InvoicePaid, paymentID: "pay_1", wantStatus: corebilling.InvoiceRefunded,
			wantEvents: []string{"paid>refunded refund rf_1"},
		},
		"a partial refund": {
			status: corebilling.InvoicePaid, paymentID: "pay_1", partial: true, wantStatus: corebilling.InvoicePaid,
			wantEvents: []string{"paid>paid partial refund rf_1 of $2.50"},
		},
		"a refund of an invoice not yet paid": {
			status: corebilling.InvoiceCharged, paymentID: "pay_1", wantStatus: corebilling.InvoiceCharged, rejected: true,
		},
		"a refund an invoice already took": {
			status: corebilling.InvoiceRefunded, paymentID: "pay_1", wantStatus: corebilling.InvoiceRefunded,
		},
		"a refund of a payment no invoice holds": {
			status: corebilling.InvoicePaid, paymentID: "pay_0", wantStatus: corebilling.InvoicePaid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharged, tc.paymentID, closeNow)
			updateInvoice(t, f, id, "status = '"+string(tc.status)+"'")

			deliverPayment(t, f, provider, "evt_1", corebilling.PaymentEvent{
				Payment: corebilling.Payment{PaymentID: "pay_1"}, PartialRefund: tc.partial, RefundCents: 250, RefundID: "rf_1",
			})
			if state := chargeStateOf(t, f, id); state.status != string(tc.wantStatus) {
				t.Errorf("invoice = %+v, want %s", state, tc.wantStatus)
			}
			if got := transitions(t, f, id)[2:]; !slices.Equal(got, tc.wantEvents) {
				t.Errorf("events = %q, want %q", got, tc.wantEvents)
			}
			if d := storedDelivery(t, f, "evt_1"); !d.ProcessedAt.Valid || (d.Error != "") != tc.rejected {
				t.Errorf("delivery processed=%v error=%q, want processed and rejected=%v",
					d.ProcessedAt.Valid, d.Error, tc.rejected)
			}
		})
	}
}

// fetchAsProvider reads every payment back as another, as a changed response shape would.
type fetchAsProvider struct{ *fakeProvider }

func (fetchAsProvider) FetchPayment(context.Context, string) (corebilling.Payment, error) {
	return corebilling.Payment{PaymentID: "pay_other", Status: corebilling.PaymentSucceeded}, nil
}

// A read that fails, or that cannot be trusted, settles nothing and is counted.
func TestSettleCountsWhatItCannotRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]func(f *fixture, provider *fakeProvider, id string) *corebilling.Service{
		"a listing that fails": func(f *fixture, provider *fakeProvider, _ string) *corebilling.Service {
			provider.listErr = errors.New("provider unreachable")
			return f.svc
		},
		"a payment that reads back as another": func(f *fixture, provider *fakeProvider, _ string) *corebilling.Service {
			return f.svcWithProvider(t, fetchAsProvider{provider})
		},
		"a charge another provider made": func(f *fixture, _ *fakeProvider, id string) *corebilling.Service {
			updateInvoice(t, f, id, "provider = 'other'")
			return f.svc
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
			provider.payments = []corebilling.Payment{paymentOf("pay_1", id, subID, corebilling.PaymentSucceeded, closeNow)}
			svc := tc(f, provider, id)

			if r := settleCharges(t, svc, settleNow); r != (corebilling.SettleReport{Unreadable: 1}) {
				t.Errorf("report = %+v, want one unreadable", r)
			}
			if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceCharging) ||
				len(transitions(t, f, id)) != 1 {
				t.Errorf("invoice = %+v, want left charging with nothing recorded", state)
			}
		})
	}
}

// Unsettled charges with no provider to read them fail the step; billing off reads nothing.
func TestSettleWithoutAProviderOrBilling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	t.Run("no provider", func(t *testing.T) {
		f := newFixture(t)
		seedCharge(t, f, periodStart, "sub_1", corebilling.InvoiceCharging, "", closeNow)
		if r := settleCharges(t, f.svc, closeNow.Add(time.Minute)); r != (corebilling.SettleReport{}) {
			t.Errorf("before it is due: report = %+v, want nothing", r)
		}
		if _, err := f.svc.SettleCharges(t.Context(), settleNow); !errors.Is(err, corebilling.ErrNoProvider) {
			t.Errorf("err = %v, want ErrNoProvider", err)
		}
	})
	t.Run("billing off", func(t *testing.T) {
		f, provider := newPaidFixture(t)
		subID := seedMandate(t, f, periodStart, time.Time{})
		id := seedCharge(t, f, periodStart, subID, corebilling.InvoiceCharging, "", closeNow)
		svc, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, &corebilling.Payments{
			MandateProduct: mandateProduct, Provider: provider,
		})
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		if r := settleCharges(t, svc, settleNow); r != (corebilling.SettleReport{}) {
			t.Errorf("report = %+v, want nothing", r)
		}
		if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceCharging) {
			t.Errorf("invoice = %+v, want untouched", state)
		}
	})
}
