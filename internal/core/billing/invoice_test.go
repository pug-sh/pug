package billing_test

import (
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/rs/xid"
)

// The fixture org is created 2025-03-10, so its periods run 10th to 10th. At this
// instant the period 10 May - 10 Jun is due (two days of grace have passed) and
// the period 10 Jun - 10 Jul is running.
var invoiceNow = time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

var (
	periodStart = time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// The pin the pass wants for the running period: its end, the grace, a day.
	wantPin = time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func seedProject(t *testing.T, f *fixture) string {
	t.Helper()
	id := xid.New().String()
	project, err := dbwrite.New(f.pg.PgW).CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: id, OrgID: f.orgID, DisplayName: "project-" + id,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return project.ID
}

func seedUsage(t *testing.T, f *fixture, projectID string, on time.Time, count int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_daily (day, event_count, org_id, project_id) values ($1, $2, $3, $4)
		 on conflict (project_id, day) do update set event_count = excluded.event_count`,
		on, count, f.orgID, projectID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// stampUsage records that the meter ran for the org at `at`, on the period it
// is keeping current.
func stampUsage(t *testing.T, f *fixture, orgID string, start, end time.Time, count int64, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_periods (event_count, org_id, period_end, period_start, usage_computed_at)
		 values ($1, $2, $3, $4, $5)
		 on conflict (org_id, period_start) do update
		 set event_count = excluded.event_count, usage_computed_at = excluded.usage_computed_at`,
		count, orgID, end, start, at); err != nil {
		t.Fatalf("stamp usage: %v", err)
	}
}

// seedMandate stores a live on-demand mandate first seen at `since`, with its
// next billing date already where the pass would pin it.
func seedMandate(t *testing.T, f *fixture, subID string, since time.Time) {
	t.Helper()
	seedMandateWith(t, f, subID, since, "active", wantPin, false)
}

func seedMandateWith(t *testing.T, f *fixture, subID string, since time.Time, status string, periodEnd time.Time, cancelAtEnd bool) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   cancel_at_period_end, create_time, currency, current_period_end, id, on_demand, org_id, plan_slug,
		   price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ($1, $2, 'USD', $3, $4, true, $5, $6, 0, $7, 'cus_1', $8, $4, $2, $8)`,
		cancelAtEnd, since, periodEnd, subID, f.orgID, corebilling.CurrentSlug, fakeProviderName, status); err != nil {
		t.Fatalf("seed mandate: %v", err)
	}
}

// backdate moves an invoice's update_time behind the moddatetime trigger, which
// otherwise resets it on every update.
func backdate(t *testing.T, f *fixture, id string, to time.Time) {
	t.Helper()
	for _, q := range []string{
		`alter table billing_invoices disable trigger update_timestamp`,
		`update billing_invoices set update_time = $2 where id = $1`,
		`alter table billing_invoices enable trigger update_timestamp`,
	} {
		if _, err := f.pg.PgW.Exec(t.Context(), q, id, to); err != nil && q[:6] == "update" {
			t.Fatalf("backdate invoice: %v", err)
		} else if err != nil {
			if _, err := f.pg.PgW.Exec(t.Context(), q); err != nil {
				t.Fatalf("toggle trigger: %v", err)
			}
		}
	}
}

func invoices(t *testing.T, f *fixture) []corebilling.Invoice {
	t.Helper()
	out, err := f.svc.ListInvoices(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	return out
}

func onlyInvoice(t *testing.T, f *fixture) corebilling.Invoice {
	t.Helper()
	all := invoices(t, f)
	if len(all) != 1 {
		t.Fatalf("got %d invoices, want 1: %+v", len(all), all)
	}
	return all[0]
}

func pass(t *testing.T, f *fixture, now time.Time) corebilling.InvoiceReport {
	t.Helper()
	report, err := f.svc.InvoicePass(t.Context(), now)
	if err != nil {
		t.Fatalf("InvoicePass: %v", err)
	}
	return report
}

// The first invoice of a mandate covers only the days from the day the card was
// added, at the card the mandate pinned, and one pass charges it. Every other
// mandate in this file is seeded a day into the period, so it overlaps exactly
// one closable period.
func TestCloseBillsTheMandatesDaysAndCharges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 20))
	seedUsage(t, f, project, day(2026, time.May, 15), 1_000_000)
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)

	report := pass(t, f, invoiceNow)
	if report.Closed != 1 || report.Charged != 1 {
		t.Fatalf("report = %+v, want 1 closed and 1 charged", report)
	}
	inv := onlyInvoice(t, f)
	if inv.EventCount != 2_340_000 || inv.Blocks != 23 || inv.AmountCents != 9_700 {
		t.Errorf("invoice = %d events, %d blocks, %d cents; want 2340000, 23, 9700", inv.EventCount, inv.Blocks, inv.AmountCents)
	}
	if !inv.BilledFrom.Equal(day(2026, time.May, 20)) || !inv.BilledTo.Equal(periodEnd) {
		t.Errorf("billed [%s, %s), want the mandate's days", inv.BilledFrom, inv.BilledTo)
	}
	if !inv.PeriodStart.Equal(periodStart) || !inv.PeriodEnd.Equal(periodEnd) {
		t.Errorf("period [%s, %s)", inv.PeriodStart, inv.PeriodEnd)
	}
	if inv.Status != corebilling.InvoiceCharged || inv.ProviderPaymentID != "pay_1" || inv.Attempts != 1 {
		t.Errorf("invoice = %s/%s/%d attempts, want charged with pay_1 after one attempt", inv.Status, inv.ProviderPaymentID, inv.Attempts)
	}
	if inv.PlanSlug != corebilling.CurrentSlug || inv.Pricing.Card == nil {
		t.Errorf("priced on %q/%v, want the pinned card", inv.PlanSlug, inv.Pricing.Card)
	}
	if len(provider.charges) != 1 || provider.charges[0].AmountCents != 9_700 || provider.charges[0].InvoiceID != inv.ID {
		t.Errorf("charges = %+v, want one for the invoice's amount", provider.charges)
	}

	// A second pass closes nothing twice: the unique index is the guard.
	if again := pass(t, f, invoiceNow.Add(time.Hour)); again.Closed != 0 || again.Charged != 0 {
		t.Errorf("second pass = %+v, want nothing new", again)
	}
	onlyInvoice(t, f)
}

// A stalled meter delays an invoice, never mis-bills one.
func TestCloseIsHeldByAStaleMeter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	stampUsage(t, f, f.orgID, periodStart, periodEnd, 0, day(2026, time.June, 11))

	report := pass(t, f, invoiceNow)
	if report.Held != 1 || report.Closed != 0 {
		t.Errorf("report = %+v, want 1 held", report)
	}
	if got := invoices(t, f); len(got) != 0 {
		t.Errorf("invoices = %+v, want none while the count is not final", got)
	}
}

// Under the minimum the invoice is recorded and never sent: a month inside the
// free block, or a period entirely inside the trial.
func TestCloseWaivesUnderTheMinimum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 100_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)

	report := pass(t, f, invoiceNow)
	if report.Waived != 1 || report.Charged != 0 {
		t.Errorf("report = %+v, want 1 waived", report)
	}
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceWaived || inv.AmountCents != 0 || inv.Blocks != 1 {
		t.Errorf("invoice = %s %d cents %d blocks, want waived at 0 cents for the free block", inv.Status, inv.AmountCents, inv.Blocks)
	}
	if len(provider.charges) != 0 {
		t.Error("a waived invoice reached the provider")
	}
}

// Trial days are never billed, and a mandate added on the 28th does not pay for
// the 27 days before it.
func TestCloseClipsTheTrial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	// A young org: created 25 May, trial through 8 June, anchored on the 25th.
	created := day(2026, time.May, 25)
	if _, err := f.pg.PgW.Exec(t.Context(), `update orgs set create_time = $2 where id = $1`, f.orgID, created); err != nil {
		t.Fatalf("backdate org: %v", err)
	}
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", created)
	seedUsage(t, f, project, day(2026, time.May, 30), 1_000_000)
	seedUsage(t, f, project, day(2026, time.June, 9), 1_000_000)
	now := day(2026, time.June, 28)
	stampUsage(t, f, f.orgID, day(2026, time.June, 25), day(2026, time.July, 25), 0, now)

	pass(t, f, now)
	inv := onlyInvoice(t, f)
	if !inv.BilledFrom.Equal(day(2026, time.June, 8)) || !inv.BilledTo.Equal(day(2026, time.June, 25)) {
		t.Errorf("billed [%s, %s), want from the first full day after the trial", inv.BilledFrom, inv.BilledTo)
	}
	if inv.EventCount != 1_000_000 || inv.AmountCents != 4_500 {
		t.Errorf("invoice = %d events %d cents, want only the post-trial million at 4500", inv.EventCount, inv.AmountCents)
	}
}

// The last invoice of a mandate covers only the days before it was removed.
func TestCloseClipsACancelledMandate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandateWith(t, f, "sub_1", day(2026, time.May, 11), "cancelled", wantPin, false)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set ended_at = $1 where provider_sub_id = 'sub_1'`, day(2026, time.May, 20).Add(14*time.Hour)); err != nil {
		t.Fatalf("end the mandate: %v", err)
	}
	seedUsage(t, f, project, day(2026, time.May, 15), 1_000_000)
	seedUsage(t, f, project, day(2026, time.May, 25), 1_000_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)

	report := pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	if !inv.BilledTo.Equal(day(2026, time.May, 20)) || inv.EventCount != 1_000_000 {
		t.Errorf("billed to %s with %d events, want the days before the cancellation only", inv.BilledTo, inv.EventCount)
	}
	// Nothing left to charge it against: a finding, not a retry loop.
	if inv.Status != corebilling.InvoiceUncollectible || inv.LastErrorCode != "mandate_gone" || report.MandateGone != 1 {
		t.Errorf("invoice = %s/%s report=%+v, want uncollectible for a gone mandate", inv.Status, inv.LastErrorCode, report)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status != corebilling.StatusPastDue {
		t.Errorf("status = %s, want PAST_DUE from the ledger", ent.Status)
	}
}

// A free org gets no invoice row; a deal is invoiced whether or not a mandate
// exists, and reported when nothing can charge it.
func TestCloseSkipsFreeOrgsAndInvoicesDeals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	project := seedProject(t, f)
	seedUsage(t, f, project, day(2026, time.May, 25), 5_000_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)

	if report := pass(t, f, invoiceNow); report.Closed != 0 || report.Waived != 0 {
		t.Errorf("report = %+v, want nothing for a free org", report)
	}
	if got := invoices(t, f); len(got) != 0 {
		t.Errorf("a free org got invoices: %+v", got)
	}

	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug: corebilling.SlugCustom, FlatFeeCents: new(int64(40_000)),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	// Recorded mid-period: the running period is the deal's first, and the ones
	// before it are never backfilled.
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set create_time = $2 where org_id = $1`, f.orgID, day(2026, time.May, 15)); err != nil {
		t.Fatalf("backdate the deal: %v", err)
	}
	report := pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	if inv.AmountCents != 40_000 || inv.PlanSlug != corebilling.SlugCustom || inv.Pricing.Terms == nil {
		t.Errorf("invoice = %d cents on %q, want the flat fee on custom terms", inv.AmountCents, inv.PlanSlug)
	}
	if inv.Status != corebilling.InvoiceUncollectible || report.MandateGone != 1 {
		t.Errorf("invoice = %s report=%+v, want uncollectible with no mandate", inv.Status, report)
	}
}

// The charge that times out after the provider created the payment: settled by
// LISTING, and never charged twice.
func TestAmbiguousChargeIsSettledByReading(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(in corebilling.ChargeInput) (string, error) {
		provider.recordPayment(in, corebilling.PaymentPending)
		return "", errors.New("connection reset")
	}

	report := pass(t, f, invoiceNow)
	if report.Ambiguous != 1 {
		t.Fatalf("report = %+v, want 1 ambiguous", report)
	}
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceCharging {
		t.Fatalf("status = %s, want charging until it is settled by reading", inv.Status)
	}

	backdate(t, f, inv.ID, invoiceNow.Add(-10*time.Minute))
	report = pass(t, f, invoiceNow.Add(time.Hour))
	inv = onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceCharged || inv.ProviderPaymentID != "pay_1" || report.Settled != 1 {
		t.Errorf("invoice = %s/%s report=%+v, want charged with the payment found by listing", inv.Status, inv.ProviderPaymentID, report)
	}
	if len(provider.charges) != 1 {
		t.Errorf("charged %d times, want 1 — an ambiguous outcome must be resolved by reading", len(provider.charges))
	}

	// Polled once old enough, and paid once the provider says so.
	provider.settlePayment("pay_1", corebilling.PaymentSucceeded, "")
	backdate(t, f, inv.ID, invoiceNow.Add(-2*time.Hour))
	pass(t, f, invoiceNow.Add(2*time.Hour))
	inv = onlyInvoice(t, f)
	if inv.Status != corebilling.InvoicePaid || inv.ProviderInvoiceURL == "" {
		t.Errorf("invoice = %s url=%q, want paid with the receipt", inv.Status, inv.ProviderInvoiceURL)
	}
}

// The charge that never arrived: reopened after a read finds no payment, and the
// next tick charges again — once.
func TestAmbiguousChargeWithNoPaymentIsReopened(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) { return "", errors.New("timeout") }

	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	backdate(t, f, inv.ID, invoiceNow.Add(-10*time.Minute))
	provider.charge = nil

	// Charge runs before settle, so the reopen lands this pass and the retry the next.
	report := pass(t, f, invoiceNow.Add(time.Hour))
	if report.Reopened != 1 {
		t.Fatalf("report = %+v, want 1 reopened", report)
	}
	// The attempt is counted: a charge was POSTed, and that is what bounds the
	// charging -> open cycle at max_charge_attempts.
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceOpen || inv.Attempts != 1 {
		t.Fatalf("invoice = %s/%d attempts, want open with the attempt counted", inv.Status, inv.Attempts)
	}
	pass(t, f, invoiceNow.Add(2*time.Hour))
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceCharged || inv.Attempts != 2 {
		t.Errorf("invoice = %s/%d attempts, want charged on the retry", inv.Status, inv.Attempts)
	}
	if len(provider.charges) != 2 || len(provider.payments) != 1 {
		t.Errorf("%d charges, %d payments; want 2 and 1", len(provider.charges), len(provider.payments))
	}
}

// Dunning: a soft decline retries at +3d, +7d, +7d and then gives up; the org is
// PAST_DUE throughout and nothing is enforced.
func TestSoftDeclineRetriesOnTheScheduleThenGivesUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", &corebilling.DeclineError{Code: "INSUFFICIENT_FUNDS", Message: "insufficient funds"}
	}

	at := invoiceNow
	for i, wait := range []time.Duration{3 * 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour} {
		report := pass(t, f, at)
		if report.Declined != 1 {
			t.Fatalf("attempt %d: report = %+v, want 1 declined", i+1, report)
		}
		inv := onlyInvoice(t, f)
		if inv.Status != corebilling.InvoiceFailed || inv.Attempts != i+1 || inv.LastErrorCode != "INSUFFICIENT_FUNDS" {
			t.Fatalf("attempt %d: invoice = %s/%d/%s", i+1, inv.Status, inv.Attempts, inv.LastErrorCode)
		}
		if want := at.Add(wait); !inv.NextAttemptAt.Equal(want) {
			t.Errorf("attempt %d: next_attempt_at = %s, want %s", i+1, inv.NextAttemptAt, want)
		}
		ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, at)
		if err != nil {
			t.Fatalf("GetEntitlement: %v", err)
		}
		if ent.Status != corebilling.StatusPastDue || !ent.Chargeable {
			t.Errorf("attempt %d: status = %s chargeable=%v, want PAST_DUE and still chargeable", i+1, ent.Status, ent.Chargeable)
		}
		// Too early: nothing is retried before its date.
		if early := pass(t, f, at.Add(wait-time.Hour)); early.Declined != 0 {
			t.Errorf("attempt %d: an early pass retried", i+1)
		}
		at = at.Add(wait)
	}
	pass(t, f, at)
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceUncollectible || inv.Attempts != 4 {
		t.Errorf("after the fourth failure: %s/%d attempts, want uncollectible after 4", inv.Status, inv.Attempts)
	}
	if len(provider.charges) != 4 {
		t.Errorf("charged %d times, want 4", len(provider.charges))
	}
}

// A hard decline is uncollectible at once: retrying only damages authorization
// rates. A new card re-opens it, and the next pass charges.
func TestHardDeclineIsUncollectibleUntilANewCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", &corebilling.DeclineError{Code: "STOLEN_CARD"}
	}

	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceUncollectible || inv.Attempts != 1 {
		t.Fatalf("invoice = %s/%d, want uncollectible after one attempt", inv.Status, inv.Attempts)
	}
	if later := pass(t, f, invoiceNow.AddDate(0, 0, 30)); later.Declined != 0 {
		t.Error("an uncollectible invoice was retried")
	}

	provider.charge = nil
	event := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	event.PaymentMethodUpdated = true
	provider.event = event
	newCard := invoiceNow.AddDate(0, 0, 31)
	d := delivery("evt_card", newCard)
	d.EventType = "subscription.update_payment_method"
	if err := f.svc.HandleDelivery(t.Context(), provider, d); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceOpen || !inv.NextAttemptAt.Equal(newCard) {
		t.Fatalf("invoice = %s next=%s, want open now after a new card", inv.Status, inv.NextAttemptAt)
	}
	pass(t, f, newCard)
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceCharged {
		t.Errorf("invoice = %s, want charged on the new card", inv.Status)
	}
}

// payment.succeeded and payment.failed settle by metadata.invoice_id through the
// same inbox; a refund moves a paid invoice to refunded.
func TestPaymentWebhooksSettleTheInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)

	send := func(id, eventType string, pe corebilling.PaymentEvent) {
		t.Helper()
		provider.payment = pe
		d := delivery(id, invoiceNow.Add(time.Minute))
		d.EventType = eventType
		if err := f.svc.HandleDelivery(t.Context(), provider, d); err != nil {
			t.Fatalf("HandleDelivery(%s): %v", eventType, err)
		}
	}

	send("evt_failed", "payment.failed", corebilling.PaymentEvent{Payment: corebilling.PaymentRecord{
		PaymentID: "pay_1", InvoiceID: inv.ID, Status: corebilling.PaymentFailed, ErrorCode: "INSUFFICIENT_FUNDS", ErrorMessage: "try later",
	}})
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceFailed || inv.Attempts != 1 || inv.LastErrorCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("after payment.failed: %s/%d/%s", inv.Status, inv.Attempts, inv.LastErrorCode)
	}
	if inv.LastErrorMessage != "try later" {
		t.Errorf("last_error_message = %q, want the provider's stored for the operator", inv.LastErrorMessage)
	}

	// The retry succeeds through the webhook rather than the poll.
	pass(t, f, inv.NextAttemptAt)
	send("evt_paid", "payment.succeeded", corebilling.PaymentEvent{Payment: corebilling.PaymentRecord{
		PaymentID: "pay_2", InvoiceID: inv.ID, Status: corebilling.PaymentSucceeded, InvoiceURL: "https://pay.example/receipt/2",
	}})
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoicePaid || inv.ProviderInvoiceURL != "https://pay.example/receipt/2" {
		t.Fatalf("after payment.succeeded: %s url=%q", inv.Status, inv.ProviderInvoiceURL)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Status == corebilling.StatusPastDue {
		t.Error("PAST_DUE did not clear once the retry succeeded")
	}

	send("evt_refund", "refund.succeeded", corebilling.PaymentEvent{Refund: true, Payment: corebilling.PaymentRecord{PaymentID: "pay_2"}})
	if inv = onlyInvoice(t, f); inv.Status != corebilling.InvoiceRefunded {
		t.Errorf("after refund.succeeded: %s, want refunded", inv.Status)
	}
}

// The estimate carries the meter's three states through: an unknown count is
// never rendered as $0.
func TestUpcomingInvoiceCarriesFreshness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))

	up, err := f.svc.UpcomingInvoice(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("UpcomingInvoice: %v", err)
	}
	if up.Counted || up.Priced || !up.UsageComputedAt.IsZero() {
		t.Errorf("never metered: %+v, want nothing counted and nothing priced", up)
	}
	if !up.NextChargeAt.Equal(periodEnd.AddDate(0, 1, 2)) {
		t.Errorf("next_charge_at = %s, want the running period's end plus the grace", up.NextChargeAt)
	}

	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 2_340_000, invoiceNow)
	up, err = f.svc.UpcomingInvoice(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("UpcomingInvoice: %v", err)
	}
	if !up.Counted || !up.Priced || up.EventCount != 2_340_000 || up.Quote.TotalCents != 9_700 {
		t.Errorf("metered: %+v, want 2340000 events priced at 9700", up)
	}
}

// void stops a charge before it happens; retry puts an uncollectible invoice
// back in line. Neither touches a paid one.
func TestVoidAndRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", &corebilling.DeclineError{Code: "DO_NOT_HONOR"}
	}
	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)

	if _, err := f.svc.RetryInvoice(t.Context(), inv.ID, "", invoiceNow); !errors.Is(err, corebilling.ErrActorRequired) {
		t.Errorf("retry with no actor = %v, want ErrActorRequired", err)
	}
	retried, err := f.svc.RetryInvoice(t.Context(), inv.ID, actor, invoiceNow)
	if err != nil {
		t.Fatalf("RetryInvoice: %v", err)
	}
	if retried.Status != corebilling.InvoiceOpen {
		t.Errorf("retried = %s, want open", retried.Status)
	}
	voided, err := f.svc.VoidInvoice(t.Context(), inv.ID, actor, "wrong customer")
	if err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if voided.Status != corebilling.InvoiceVoid {
		t.Errorf("voided = %s, want void", voided.Status)
	}
	if _, err := f.svc.VoidInvoice(t.Context(), inv.ID, actor, "again"); !errors.Is(err, corebilling.ErrInvoiceTransition) {
		t.Errorf("void twice = %v, want ErrInvoiceTransition", err)
	}
	if _, err := f.svc.RetryInvoice(t.Context(), xid.New().String(), actor, invoiceNow); !errors.Is(err, corebilling.ErrInvoiceNotFound) {
		t.Errorf("retry unknown = %v, want ErrInvoiceNotFound", err)
	}
	if later := pass(t, f, invoiceNow.Add(time.Hour)); later.Charged+later.Declined != 0 {
		t.Error("a void invoice was charged")
	}
}

// A cancellation scheduled in the portal closes the running period to date, so
// it is charged while the mandate is still live.
func TestScheduledCancellationClosesThePeriodEarly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandateWith(t, f, "sub_1", day(2026, time.May, 11), "active", wantPin, true)
	seedUsage(t, f, project, day(2026, time.June, 10), 500_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 500_000, invoiceNow)

	report := pass(t, f, invoiceNow)
	var early *corebilling.Invoice
	for _, inv := range invoices(t, f) {
		if inv.PeriodStart.Equal(periodEnd) {
			early = &inv
		}
	}
	if early == nil {
		t.Fatalf("no invoice for the running period: %+v", invoices(t, f))
	}
	if !early.BilledTo.Equal(day(2026, time.June, 11)) || early.EventCount != 500_000 || early.AmountCents != 2_000 {
		t.Errorf("early close = to %s, %d events, %d cents; want the final days priced", early.BilledTo, early.EventCount, early.AmountCents)
	}
	if early.Status != corebilling.InvoiceCharged {
		t.Errorf("early close = %s, want charged while the mandate is live", early.Status)
	}
	// The pin is left alone, or the cancellation would never arrive.
	if len(provider.pinned) != 0 || report.Pinned != 0 {
		t.Errorf("pinned %v on a mandate scheduled to cancel", provider.pinned)
	}
}

// Pug's own cancellation: close the period to date, charge it, then cancel.
func TestRemovePaymentMethodChargesBeforeCancelling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.June, 10), 500_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 500_000, invoiceNow)
	cancelled := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusCancelled)
	cancelled.EndedAt = invoiceNow
	provider.event = cancelled

	if err := f.svc.RemovePaymentMethod(t.Context(), f.orgID, invoiceNow); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceCharged || inv.AmountCents != 2_000 {
		t.Errorf("invoice = %s %d cents, want charged before the cancellation", inv.Status, inv.AmountCents)
	}
	if len(provider.cancelled) != 1 || provider.cancelled[0] != "sub_1" {
		t.Errorf("cancelled = %v, want sub_1", provider.cancelled)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Chargeable {
		t.Error("still chargeable after removing the payment method")
	}
	if err := f.svc.RemovePaymentMethod(t.Context(), f.orgID, invoiceNow); !errors.Is(err, corebilling.ErrNoMandate) {
		t.Errorf("second removal = %v, want ErrNoMandate", err)
	}
}

// The pass pins each mandate's next billing date just past pug's next charge,
// and re-reads the provider rather than editing the mirror by hand.
func TestPassPinsTheNextBillingDate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	seedMandateWith(t, f, "sub_1", day(2026, time.May, 11), "active", day(2026, time.July, 1), false)
	pinned := subEvent(f.orgID, "sub_1", "prod_mandate", corebilling.SubStatusActive)
	pinned.CurrentPeriodEnd = wantPin
	provider.event = pinned

	report := pass(t, f, invoiceNow)
	if report.Pinned != 1 || len(provider.pinned) != 1 || !provider.pinned[0].Equal(wantPin) {
		t.Fatalf("report=%+v pinned=%v, want one pin at %s", report, provider.pinned, wantPin)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, invoiceNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if !ent.SubPeriodEnd.Equal(wantPin) {
		t.Errorf("mirror period end = %s, want the pinned %s", ent.SubPeriodEnd, wantPin)
	}
	if again := pass(t, f, invoiceNow.Add(time.Hour)); again.Pinned != 0 {
		t.Error("a mandate already pinned was pinned again")
	}
}

// What the free tier costs: a closed period over the allowance with no mandate
// is reported and never billed.
func TestPassReportsUnbilledUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)
	stampUsage(t, f, f.orgID, periodStart, periodEnd, 500_000, invoiceNow)

	report := pass(t, f, invoiceNow)
	if report.Unbilled != 1 {
		t.Errorf("unbilled = %d, want 1", report.Unbilled)
	}
	if got := invoices(t, f); len(got) != 0 {
		t.Errorf("unbilled usage produced invoices: %+v", got)
	}
}

// ListPayments promises no order, and a retry after a decline shares the invoice
// id with the attempt that failed. Settling on the failed one duns a customer
// whose card worked and leaves the successful charge unrecorded.
func TestSettleTakesTheNewestSucceededPayment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)

	// The provider answers with the older failure first.
	provider.charge = func(in corebilling.ChargeInput) (string, error) {
		provider.payments = append(provider.payments,
			corebilling.PaymentRecord{
				PaymentID: "pay_old", InvoiceID: in.InvoiceID, Status: corebilling.PaymentFailed,
				ErrorCode: "INSUFFICIENT_FUNDS", CreatedAt: invoiceNow.Add(-time.Hour),
			},
			corebilling.PaymentRecord{
				PaymentID: "pay_new", InvoiceID: in.InvoiceID, Status: corebilling.PaymentSucceeded,
				CreatedAt: invoiceNow,
			},
		)
		return "", errors.New("connection reset")
	}

	report := pass(t, f, invoiceNow)
	if report.Ambiguous != 1 {
		t.Fatalf("report = %+v, want 1 ambiguous", report)
	}
	inv := onlyInvoice(t, f)
	backdate(t, f, inv.ID, invoiceNow.Add(-10*time.Minute))
	pass(t, f, invoiceNow.Add(time.Hour))

	inv = onlyInvoice(t, f)
	if inv.Status != corebilling.InvoicePaid || inv.ProviderPaymentID != "pay_new" {
		t.Errorf("invoice = %s/%s, want paid on pay_new — the succeeded payment, not the earlier failure",
			inv.Status, inv.ProviderPaymentID)
	}
	if len(provider.charges) != 1 {
		t.Errorf("charged %d times, want 1", len(provider.charges))
	}
}

// Unreadable is the counter that fails the CronJob, and nothing exercised it: a
// provider that cannot be read must not look like a clean pass.
func TestAProviderThatCannotBeReadIsCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", errors.New("connection reset")
	}

	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	backdate(t, f, inv.ID, invoiceNow.Add(-10*time.Minute))

	provider.listErr = errors.New("dodo is down")
	report := pass(t, f, invoiceNow.Add(time.Hour))
	if report.Unreadable != 1 {
		t.Fatalf("report = %+v, want 1 unreadable", report)
	}
	if got := onlyInvoice(t, f); got.Status != corebilling.InvoiceCharging {
		t.Errorf("status = %s, want charging — an unreadable provider decides nothing", got.Status)
	}
}

// A charge whose payment can never be read must not cycle charging -> open
// forever: every lap POSTs a charge that may really take money.
func TestAnUnsettleableChargeStopsAtTheAttemptCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	// Charges "succeed" but the payment is never visible to a read.
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", errors.New("connection reset")
	}

	at := invoiceNow
	for range 10 {
		inv := onlyInvoice(t, f)
		if inv.Status == corebilling.InvoiceUncollectible {
			break
		}
		backdate(t, f, inv.ID, at.Add(-10*time.Minute))
		at = at.Add(time.Hour)
		pass(t, f, at)
	}

	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceUncollectible {
		t.Fatalf("invoice = %s after ten passes, want uncollectible — the cycle is unbounded", inv.Status)
	}
	if len(provider.charges) > 4 {
		t.Errorf("charged %d times, want at most %d", len(provider.charges), 4)
	}
}

// Void stops a charge before it happens. Voiding one in flight would drop the
// row out of settle's scan, so a payment that succeeded is never reconciled.
func TestVoidRefusesAChargeInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	project := seedProject(t, f)
	seedMandate(t, f, "sub_1", day(2026, time.May, 11))
	seedUsage(t, f, project, day(2026, time.May, 25), 2_340_000)
	stampUsage(t, f, f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), 0, invoiceNow)
	provider.charge = func(corebilling.ChargeInput) (string, error) {
		return "", errors.New("connection reset")
	}

	pass(t, f, invoiceNow)
	inv := onlyInvoice(t, f)
	if inv.Status != corebilling.InvoiceCharging {
		t.Fatalf("status = %s, want charging", inv.Status)
	}
	if _, err := f.svc.VoidInvoice(t.Context(), inv.ID, "praveen", "customer asked"); !errors.Is(err, corebilling.ErrInvoiceTransition) {
		t.Errorf("void a charging invoice = %v, want ErrInvoiceTransition", err)
	}
}
