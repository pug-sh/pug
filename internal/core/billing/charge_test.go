package billing_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/rs/xid"
)

// seedDue stores an open invoice an earlier close left, due a charge at due.
func seedDue(t *testing.T, f *fixture, from time.Time, cents int64, due time.Time) string {
	t.Helper()
	id := xid.New().String()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, currency, event_count, id, lines, next_attempt_at,
		   org_id, period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
		 values ($1, $2, $3, 'USD', 1000000, $4, '[]', $5, $6, $7, $8, 'x', '{}', 'open', $1, now())`,
		cents, from, from.AddDate(0, 1, 0), id, due, f.orgID, from.AddDate(0, 1, 0), from); err != nil {
		t.Fatalf("seed due invoice: %v", err)
	}
	return id
}

// updateInvoice moves an invoice by hand, as another writer would.
func updateInvoice(t *testing.T, f *fixture, id, set string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(), `update billing_invoices set `+set+` where id = $1`, id); err != nil {
		t.Fatalf("update invoice: %v", err)
	}
}

// seedMandateIn stores an on-demand mandate added at created and left in status.
func seedMandateIn(t *testing.T, f *fixture, created time.Time, status string) string {
	t.Helper()
	subID := "sub_" + xid.New().String()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   create_time, currency, ended_at, id, on_demand, org_id, plan_slug, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ($1, 'USD', $1, $2, true, $3, $4, 'fake', 'cus_1', $5, $6, $1, $5)`,
		created, xid.New().String(), f.orgID, currentCard().Slug, status, subID); err != nil {
		t.Fatalf("seed mandate: %v", err)
	}
	return subID
}

func chargeDue(t *testing.T, svc *corebilling.Service, now time.Time) corebilling.ChargeReport {
	t.Helper()
	r, err := svc.ChargeDue(t.Context(), now)
	if err != nil {
		t.Fatalf("ChargeDue: %v", err)
	}
	return r
}

type chargeState struct {
	status, providerSubID, paymentID, code, message string
	attempts                                        int
	failedAt, nextAttemptAt                         *time.Time
}

func chargeStateOf(t *testing.T, f *fixture, id string) chargeState {
	t.Helper()
	var s chargeState
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select status, coalesce(provider_sub_id, ''), coalesce(provider_payment_id, ''), last_error_code,
		        last_error_message, attempts, failed_at, next_attempt_at
		 from billing_invoices where id = $1`, id).Scan(&s.status, &s.providerSubID, &s.paymentID,
		&s.code, &s.message, &s.attempts, &s.failedAt, &s.nextAttemptAt); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return s
}

// transitions is an invoice's recorded moves, oldest first.
func transitions(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select rtrim(from_status || '>' || to_status || ' ' || detail)
		 from billing_invoice_events where invoice_id = $1 order by at, id`, id)
	if err != nil {
		t.Fatalf("query invoice events: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect invoice events: %v", err)
	}
	return out
}

// A closed invoice is charged once its notice window has run, once.
func TestChargeChargesAnInvoiceOnceItsNoticeHasRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, periodEnd, 100_000)
	subID := seedMandate(t, f, periodStart, time.Time{})
	stampMeter(t, f, closeNow)
	closePeriods(t, f, closeNow)
	due := closeNow.AddDate(0, 0, corebilling.ChargeNoticeDays)

	if r := chargeDue(t, f.svc, due.Add(-time.Minute)); r != (corebilling.ChargeReport{}) || len(provider.charges) != 0 {
		t.Fatalf("inside the notice window: report = %+v, charges = %d, want none", r, len(provider.charges))
	}
	if r := chargeDue(t, f.svc, due); r != (corebilling.ChargeReport{Charged: 1}) {
		t.Fatalf("report = %+v, want one charged", r)
	}
	inv := invoices(t, f)[0]
	if len(provider.charges) != 1 {
		t.Fatalf("charges = %d, want 1", len(provider.charges))
	}
	got := provider.charges[0]
	if !got.PeriodStart.Equal(periodStart) {
		t.Errorf("period_start = %s, want %s", got.PeriodStart, periodStart)
	}
	got.PeriodStart = time.Time{}
	want := corebilling.ChargeInput{
		AmountCents:   inv.amount,
		Currency:      "USD",
		Description:   "Pug: 3,100,000 events, 10 Aug to 9 Sep 2026",
		InvoiceID:     inv.id,
		OrgID:         f.orgID,
		ProviderSubID: subID,
	}
	if got != want {
		t.Errorf("charge = %+v\nwant     %+v", got, want)
	}

	state := chargeStateOf(t, f, inv.id)
	if state.status != string(corebilling.InvoiceCharged) || state.paymentID != "pay_"+inv.id ||
		state.providerSubID != subID || state.attempts != 1 {
		t.Errorf("invoice = %+v, want charged on the mandate with one attempt", state)
	}
	if got, want := transitions(t, f, inv.id), []string{">open", "open>charging mandate " + subID, "charging>charged payment pay_" + inv.id}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}

	if r := chargeDue(t, f.svc, due.Add(time.Hour)); r != (corebilling.ChargeReport{}) || len(provider.charges) != 1 {
		t.Errorf("next pass: report = %+v, charges = %d, want nothing more", r, len(provider.charges))
	}
}

// The claim is the intent: it is committed before the provider is called, and a
// pass started meanwhile finds nothing to charge.
func TestChargeCommitsTheClaimBeforeCallingTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandate(t, f, periodStart, time.Time{})
	seedDue(t, f, periodStart, 10_000, closeNow)

	var status string
	var nested corebilling.ChargeReport
	provider.onCharge = func(in corebilling.ChargeInput) (string, error) {
		if err := f.pg.PgRO.QueryRow(t.Context(),
			`select status from billing_invoices where id = $1`, in.InvoiceID).Scan(&status); err != nil {
			t.Errorf("read the claimed invoice: %v", err)
		}
		nested = chargeDue(t, f.svc, closeNow)
		return "pay_1", nil
	}
	chargeDue(t, f.svc, closeNow)
	if status != string(corebilling.InvoiceCharging) {
		t.Errorf("status seen by the provider call = %q, want charging", status)
	}
	if len(provider.charges) != 1 || nested != (corebilling.ChargeReport{}) {
		t.Errorf("charges = %d, nested report = %+v, want one charge", len(provider.charges), nested)
	}
}

// A claim that loses to another writer calls no provider.
func TestChargeSkipsAnInvoiceAnotherWriterMoved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandate(t, f, day(time.June, 1), time.Time{})
	first := seedDue(t, f, day(time.June, 10), 10_000, closeNow.Add(-time.Hour))
	second := seedDue(t, f, day(time.July, 10), 10_000, closeNow)
	provider.onCharge = func(corebilling.ChargeInput) (string, error) {
		if _, err := f.pg.PgW.Exec(t.Context(),
			`update billing_invoices set status = 'void' where id = $1`, second); err != nil {
			t.Errorf("void the second invoice: %v", err)
		}
		return "pay_1", nil
	}

	if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{Charged: 1}) {
		t.Errorf("report = %+v, want only the first charged", r)
	}
	if len(provider.charges) != 1 || provider.charges[0].InvoiceID != first {
		t.Errorf("charges = %+v, want the first invoice alone", provider.charges)
	}
	if state := chargeStateOf(t, f, second); state.status != "void" || len(transitions(t, f, second)) != 0 {
		t.Errorf("second invoice = %+v, want left void with nothing recorded", state)
	}
}

// A failed invoice dated for a retry is claimed again: the retry stamps the live
// mandate, clears the last attempt's payment and error, and keeps counting attempts.
func TestChargeRetriesAFailedInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	updateInvoice(t, f, id, `status = 'failed', attempts = 1, failed_at = now(), last_error_code = 'HTTP_402',
		last_error_message = 'card declined', provider = 'fake', provider_sub_id = 'sub_old',
		provider_payment_id = 'pay_old'`)
	var claimed chargeState
	provider.onCharge = func(in corebilling.ChargeInput) (string, error) {
		claimed = chargeStateOf(t, f, in.InvoiceID)
		return "pay_new", nil
	}

	if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{Charged: 1}) {
		t.Errorf("report = %+v, want one charged", r)
	}
	if claimed.status != string(corebilling.InvoiceCharging) || claimed.providerSubID != subID ||
		claimed.paymentID != "" || claimed.code != "" || claimed.message != "" {
		t.Errorf("claimed invoice = %+v, want charging on the live mandate with the last attempt cleared", claimed)
	}
	if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceCharged) ||
		state.paymentID != "pay_new" || state.attempts != 2 {
		t.Errorf("invoice = %+v, want charged with a second attempt", state)
	}
	if got, want := transitions(t, f, id), []string{"failed>charging mandate " + subID, "charging>charged payment pay_new"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// A decline is the card's answer: the invoice fails, and nothing charges it again
// until it is dated for a retry.
func TestChargeRecordsADecline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	provider.onCharge = func(corebilling.ChargeInput) (string, error) {
		return "", &corebilling.ChargeError{Code: "HTTP_402", Message: "card declined", Declined: true}
	}

	if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{Declined: 1}) {
		t.Errorf("report = %+v, want one declined", r)
	}
	state := chargeStateOf(t, f, id)
	if state.status != string(corebilling.InvoiceFailed) || state.attempts != 1 || state.code != "HTTP_402" ||
		state.message != "card declined" || state.failedAt == nil || !state.failedAt.Equal(closeNow) ||
		state.nextAttemptAt != nil {
		t.Errorf("invoice = %+v, want failed at the charge with one attempt and no retry dated", state)
	}
	if got, want := transitions(t, f, id), []string{"open>charging mandate " + subID, "charging>failed declined HTTP_402"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if chargeDue(t, f.svc, closeNow.Add(time.Hour)); len(provider.charges) != 1 {
		t.Errorf("charges = %d, want the decline not retried", len(provider.charges))
	}
}

// A charge pug cannot settle from its answer stays charging, so it is settled by
// reading and never charged again on a hunch.
func TestChargeLeavesAnUnknownOutcomeCharging(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		err      error
		wantCode string
	}{
		"no answer":                   {errors.New("dodo: charge subscription: context deadline exceeded"), ""},
		"a refusal that is pug's own": {&corebilling.ChargeError{Code: "HTTP_429", Message: "slow down"}, "HTTP_429"},
		"a success with no payment":   {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			provider.onCharge = func(corebilling.ChargeInput) (string, error) { return "", tc.err }

			if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{Ambiguous: 1}) {
				t.Errorf("report = %+v, want one ambiguous", r)
			}
			state := chargeStateOf(t, f, id)
			if state.status != string(corebilling.InvoiceCharging) || state.code != tc.wantCode ||
				state.message == "" || state.attempts != 0 || state.providerSubID != subID {
				t.Errorf("invoice = %+v, want charging on the mandate with code %q", state, tc.wantCode)
			}
			if chargeDue(t, f.svc, closeNow.Add(time.Hour)); len(provider.charges) != 1 {
				t.Errorf("charges = %d, want it never charged again", len(provider.charges))
			}
		})
	}
}

// The provider's answer is recorded even when the pass's deadline passes during the
// call: the money may have moved.
func TestChargeRecordsTheAnswerPastThePassDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		paymentID  string
		err        error
		want       corebilling.ChargeReport
		wantStatus corebilling.InvoiceStatus
		wantCode   string
	}{
		"a payment": {
			paymentID: "pay_1", want: corebilling.ChargeReport{Charged: 1}, wantStatus: corebilling.InvoiceCharged,
		},
		"a decline": {
			err:  &corebilling.ChargeError{Code: "HTTP_402", Message: "card declined", Declined: true},
			want: corebilling.ChargeReport{Declined: 1}, wantStatus: corebilling.InvoiceFailed, wantCode: "HTTP_402",
		},
		"a refusal that is pug's own": {
			err:  &corebilling.ChargeError{Code: "HTTP_429", Message: "slow down"},
			want: corebilling.ChargeReport{Ambiguous: 1}, wantStatus: corebilling.InvoiceCharging, wantCode: "HTTP_429",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			seedMandate(t, f, periodStart, time.Time{})
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			ctx, cancel := context.WithCancel(t.Context())
			provider.onCharge = func(corebilling.ChargeInput) (string, error) {
				cancel()
				return tc.paymentID, tc.err
			}

			if r, err := f.svc.ChargeDue(ctx, closeNow); err != nil || r != tc.want {
				t.Errorf("report = %+v, err = %v, want %+v", r, err, tc.want)
			}
			if state := chargeStateOf(t, f, id); state.status != string(tc.wantStatus) ||
				state.paymentID != tc.paymentID || state.code != tc.wantCode {
				t.Errorf("invoice = %+v, want %s with payment %q and code %q", state, tc.wantStatus, tc.paymentID, tc.wantCode)
			}
		})
	}
}

// A payment taken for an invoice another writer moved meanwhile is an error, never a
// quiet count.
func TestChargeFailsOnAPaymentItCannotRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	provider.onCharge = func(in corebilling.ChargeInput) (string, error) {
		updateInvoice(t, f, in.InvoiceID, "status = 'paid'")
		return "pay_1", nil
	}

	if r, err := f.svc.ChargeDue(t.Context(), closeNow); err == nil || r != (corebilling.ChargeReport{}) {
		t.Errorf("report = %+v, err = %v, want an error and nothing counted", r, err)
	}
	if got, want := transitions(t, f, id), []string{"open>charging mandate " + subID}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// A charge refused as not chargeable writes the invoice off only when the mandate
// also reads back ended and the invoice is not a deal's; anything else holds it.
func TestChargeActsOnANotChargeableRefusalOnlyWhenTheMandateReadsBackEnded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		status     corebilling.SubStatus
		deal       bool
		readErr    error
		wantStatus corebilling.InvoiceStatus
		want       corebilling.ChargeReport
	}{
		"reads back cancelled": {
			status: corebilling.SubStatusCancelled, wantStatus: corebilling.InvoiceUncollectible,
			want: corebilling.ChargeReport{MandateGone: 1},
		},
		"reads back expired": {
			status: corebilling.SubStatusExpired, wantStatus: corebilling.InvoiceUncollectible,
			want: corebilling.ChargeReport{MandateGone: 1},
		},
		"reads back live": {
			status: corebilling.SubStatusActive, wantStatus: corebilling.InvoiceCharging,
			want: corebilling.ChargeReport{Ambiguous: 1},
		},
		"reads back past due": {
			status: corebilling.SubStatusPastDue, wantStatus: corebilling.InvoiceCharging,
			want: corebilling.ChargeReport{Ambiguous: 1},
		},
		"reads back paused": {
			status: corebilling.SubStatusPaused, wantStatus: corebilling.InvoiceCharging,
			want: corebilling.ChargeReport{Ambiguous: 1},
		},
		"a deal's invoice reads back cancelled": {
			status: corebilling.SubStatusCancelled, deal: true, wantStatus: corebilling.InvoiceCharging,
			want: corebilling.ChargeReport{Ambiguous: 1},
		},
		"reads back a status pug has no word for": {
			status: "pending", wantStatus: corebilling.InvoiceCharging,
			want: corebilling.ChargeReport{Ambiguous: 1},
		},
		"reads back nothing": {
			wantStatus: corebilling.InvoiceCharging, want: corebilling.ChargeReport{Unreadable: 1},
		},
		"a second 404": {
			readErr:    fmt.Errorf("%w: sub", corebilling.ErrSubscriptionNotFound),
			wantStatus: corebilling.InvoiceCharging, want: corebilling.ChargeReport{Unreadable: 1},
		},
		"an unreadable mandate": {
			readErr:    errors.New("provider unreachable"),
			wantStatus: corebilling.InvoiceCharging, want: corebilling.ChargeReport{Unreadable: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newPaidFixture(t)
			subID := seedMandate(t, f, periodStart, time.Time{})
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			if tc.deal {
				updateInvoice(t, f, id, "plan_slug = 'custom'")
			}
			provider := &fetchProvider{
				fakeProvider: fakeProvider{name: fakeProviderName},
				remote:       map[string]corebilling.SubscriptionEvent{},
				fetchErr:     tc.readErr,
			}
			if tc.status != "" {
				provider.remote[subID] = corebilling.SubscriptionEvent{ProviderSubID: subID, Status: tc.status}
			}
			provider.onCharge = func(corebilling.ChargeInput) (string, error) {
				return "", &corebilling.ChargeError{Code: "HTTP_404", Message: "not found", NotChargeable: true}
			}

			if r := chargeDue(t, f.svcWithProvider(t, provider), closeNow); r != tc.want {
				t.Errorf("report = %+v, want %+v", r, tc.want)
			}
			state := chargeStateOf(t, f, id)
			wantCode := "HTTP_404"
			if tc.wantStatus == corebilling.InvoiceUncollectible {
				wantCode = "mandate_gone"
			}
			if state.status != string(tc.wantStatus) || state.code != wantCode || state.attempts != 0 {
				t.Errorf("invoice = %+v, want %s with code %s", state, tc.wantStatus, wantCode)
			}
		})
	}
}

// An org with no mandate keeps its invoices open, and they are charged oldest first
// once a card arrives (§19.15).
func TestChargeHoldsInvoicesUntilTheFirstCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	newer := seedDue(t, f, day(time.July, 10), 10_000, closeNow)
	older := seedDue(t, f, day(time.June, 10), 10_000, closeNow)

	if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{AwaitingCard: 2}) || len(provider.charges) != 0 {
		t.Fatalf("report = %+v, charges = %d, want both held", r, len(provider.charges))
	}
	for _, id := range []string{older, newer} {
		if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceOpen) {
			t.Errorf("invoice = %+v, want held open", state)
		}
	}

	seedMandate(t, f, closeNow, time.Time{})
	if r := chargeDue(t, f.svc, closeNow.Add(time.Hour)); r != (corebilling.ChargeReport{Charged: 2}) {
		t.Errorf("report = %+v, want both charged", r)
	}
	if len(provider.charges) != 2 || provider.charges[0].InvoiceID != older || provider.charges[1].InvoiceID != newer {
		t.Errorf("charges = %+v, want the older invoice first", provider.charges)
	}
}

// Without a live mandate an invoice is written off only once its mandate has ended: a
// deal's waits for a card, a paused mandate can resume, and a status pug has no word
// for proves nothing.
func TestChargeHoldsAnInvoiceWhoseMandateMayComeBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		mandates []string
		deal     bool
		want     corebilling.ChargeReport
	}{
		"a paused mandate": {
			mandates: []string{"paused"}, want: corebilling.ChargeReport{MandatePaused: 1},
		},
		"a paused mandate beside an ended one": {
			mandates: []string{"cancelled", "paused"}, want: corebilling.ChargeReport{MandatePaused: 1},
		},
		"a mandate in a status pug has no word for": {
			mandates: []string{"pending"}, want: corebilling.ChargeReport{Unreadable: 1},
		},
		"that beside an ended one": {
			mandates: []string{"cancelled", "pending"}, want: corebilling.ChargeReport{Unreadable: 1},
		},
		"a deal's invoice whose card ended": {
			mandates: []string{"cancelled"}, deal: true, want: corebilling.ChargeReport{AwaitingCard: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			for i, status := range tc.mandates {
				seedMandateIn(t, f, day(time.June, 1+i), status)
			}
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			if tc.deal {
				updateInvoice(t, f, id, "plan_slug = 'custom'")
			}

			if r := chargeDue(t, f.svc, closeNow); r != tc.want || len(provider.charges) != 0 {
				t.Errorf("report = %+v, charges = %d, want %+v and no charge", r, len(provider.charges), tc.want)
			}
			if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceOpen) || len(transitions(t, f, id)) != 0 {
				t.Errorf("invoice = %+v, want held open with nothing recorded", state)
			}
		})
	}
}

// A deal's invoice the close reported as awaiting a card is held by the charge too,
// though the org's earlier card ended (§19.15).
func TestChargeHoldsADealsInvoiceAsItsCloseReported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandate(t, f, day(time.June, 15), day(time.July, 1))
	setDealTerms(t, f, periodStart, time.Time{})
	seedDaily(t, f, seedProjectFor(t, f), periodStart, periodEnd, 100_000)
	stampMeter(t, f, closeNow)
	if r := closePeriods(t, f, closeNow); r.AwaitingCard != 1 {
		t.Fatalf("close report = %+v, want the deal's invoice awaiting a card", r)
	}

	due := closeNow.AddDate(0, 0, corebilling.ChargeNoticeDays)
	if r := chargeDue(t, f.svc, due); r != (corebilling.ChargeReport{AwaitingCard: 1}) || len(provider.charges) != 0 {
		t.Errorf("report = %+v, charges = %d, want the deal's invoice held", r, len(provider.charges))
	}
	invs := invoices(t, f)
	deal := slices.IndexFunc(invs, func(inv invoiceRow) bool { return inv.planSlug == corebilling.SlugCustom })
	if deal < 0 || invs[deal].status != string(corebilling.InvoiceOpen) {
		t.Errorf("invoices = %+v, want the deal's held open", invs)
	}
}

// An org whose only mandate has ended has nothing to charge.
func TestChargeWritesOffAnInvoiceWhoseMandateEnded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, from := range []corebilling.InvoiceStatus{corebilling.InvoiceOpen, corebilling.InvoiceFailed} {
		t.Run(string(from), func(t *testing.T) {
			f, provider := newPaidFixture(t)
			seedMandate(t, f, periodStart, day(time.August, 25))
			id := seedDue(t, f, periodStart, 10_000, closeNow)
			updateInvoice(t, f, id, "status = '"+string(from)+"'")

			if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{MandateGone: 1}) || len(provider.charges) != 0 {
				t.Errorf("report = %+v, charges = %d, want written off uncharged", r, len(provider.charges))
			}
			state := chargeStateOf(t, f, id)
			if state.status != string(corebilling.InvoiceUncollectible) || state.code != "mandate_gone" ||
				state.nextAttemptAt != nil || state.failedAt == nil {
				t.Errorf("invoice = %+v, want uncollectible as mandate_gone", state)
			}
			if got, want := transitions(t, f, id), []string{string(from) + ">uncollectible mandate_gone"}; !slices.Equal(got, want) {
				t.Errorf("events = %q, want %q", got, want)
			}
		})
	}
}

// A write-off no read corroborated never takes an invoice another charge has claimed.
func TestChargeNeverWritesOffAChargeInFlightUnread(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	subID := seedMandate(t, f, day(time.June, 1), time.Time{})
	seedDue(t, f, day(time.June, 10), 10_000, closeNow.Add(-time.Hour))
	second := seedDue(t, f, day(time.July, 10), 10_000, closeNow)
	provider.onCharge = func(corebilling.ChargeInput) (string, error) {
		// Another charger claims the second invoice, and the mandate ends meanwhile.
		updateInvoice(t, f, second, "status = 'charging'")
		if _, err := f.pg.PgW.Exec(t.Context(),
			`update billing_subscriptions set status = 'cancelled', ended_at = now() where provider_sub_id = $1`,
			subID); err != nil {
			t.Fatalf("cancel the mandate: %v", err)
		}
		return "pay_1", nil
	}

	if r := chargeDue(t, f.svc, closeNow); r != (corebilling.ChargeReport{Charged: 1}) {
		t.Errorf("report = %+v, want only the first charged", r)
	}
	if state := chargeStateOf(t, f, second); state.status != string(corebilling.InvoiceCharging) || len(transitions(t, f, second)) != 0 {
		t.Errorf("second invoice = %+v, want left charging with nothing recorded", state)
	}
}

// A charge carrying earlier periods says so on the customer's receipt.
func TestChargeDescribesACarriedBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		events []int64
		want   string
		cents  int64
	}{
		"two periods": {
			events: []int64{150_000, 150_000, 150_000},
			want:   "Pug: 150,000 events, 10 Nov to 9 Dec 2025, and $4.00 carried from 2 earlier periods",
			cents:  600,
		},
		"one period": {
			events: []int64{150_000, 250_000},
			want:   "Pug: 250,000 events, 10 Oct to 9 Nov 2025, and $2.00 carried from 1 earlier period",
			cents:  800,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			reports := closeMonths(t, f, tc.events...)
			if reports[len(reports)-1].Closed != 1 {
				t.Fatalf("reports = %+v, want the last close to carry the others", reports)
			}
			lastClose := time.Date(2025, 9, 10, 0, 0, 0, 0, time.UTC).AddDate(0, len(tc.events), 0).Add(grace).Add(6 * time.Hour)
			chargeDue(t, f.svc, lastClose.AddDate(0, 0, corebilling.ChargeNoticeDays))

			if len(provider.charges) != 1 || provider.charges[0].Description != tc.want || provider.charges[0].AmountCents != tc.cents {
				t.Errorf("charges = %+v, want %d cents described as %q", provider.charges, tc.cents, tc.want)
			}
		})
	}
}

// Two carried invoices from one period are one period on the receipt.
func TestChargeCountsCarriedPeriodsNotInvoices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandate(t, f, day(time.June, 1), time.Time{})
	carrier := seedDue(t, f, periodStart, 10_000, closeNow)
	july := day(time.July, 10)
	for _, from := range []time.Time{july, july.AddDate(0, 0, 10)} {
		if _, err := f.pg.PgW.Exec(t.Context(),
			`insert into billing_invoices (
			   amount_cents, billed_from, billed_to, covered_by, currency, event_count, id, lines, org_id,
			   period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
			 values (100, $1, $2, $3, 'USD', 0, $4, '[]', $5, $6, $7, 'x', '{}', 'deferred', 100, now())`,
			from, from.AddDate(0, 0, 10), carrier, xid.New().String(), f.orgID, periodStart, july); err != nil {
			t.Fatalf("seed a carried invoice: %v", err)
		}
	}
	updateInvoice(t, f, carrier, "carried_cents = 200, amount_cents = usage_cents + 200")

	chargeDue(t, f.svc, closeNow)
	want := "Pug: 1,000,000 events, 10 Aug to 9 Sep 2026, and $2.00 carried from 1 earlier period"
	if len(provider.charges) != 1 || provider.charges[0].Description != want {
		t.Errorf("charges = %+v, want described as %q", provider.charges, want)
	}
}

// Invoices due with no provider to charge them fail the step; nothing due is nothing
// to report.
func TestChargeWithoutAProviderFailsOnceAnInvoiceIsDue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	id := seedDue(t, f, periodStart, 10_000, closeNow)

	if r := chargeDue(t, f.svc, closeNow.Add(-time.Minute)); r != (corebilling.ChargeReport{}) {
		t.Errorf("before it is due: report = %+v, want nothing", r)
	}
	if r, err := f.svc.ChargeDue(t.Context(), closeNow); !errors.Is(err, corebilling.ErrNoProvider) || r != (corebilling.ChargeReport{}) {
		t.Errorf("report = %+v, err = %v, want ErrNoProvider", r, err)
	}
	if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceOpen) {
		t.Errorf("invoice = %+v, want untouched", state)
	}
}

// With billing off nothing is charged, whatever provider is configured.
func TestChargeWithBillingOffChargesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandate(t, f, periodStart, time.Time{})
	id := seedDue(t, f, periodStart, 10_000, closeNow)
	svc, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, &corebilling.Payments{
		MandateProduct: mandateProduct,
		Provider:       provider,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if r := chargeDue(t, svc, closeNow); r != (corebilling.ChargeReport{}) || len(provider.charges) != 0 {
		t.Errorf("report = %+v, charges = %d, want nothing", r, len(provider.charges))
	}
	if state := chargeStateOf(t, f, id); state.status != string(corebilling.InvoiceOpen) {
		t.Errorf("invoice = %+v, want untouched", state)
	}
}
