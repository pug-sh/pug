package billing_test

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/rs/xid"
)

func nextCharge(t *testing.T, f *fixture) time.Time {
	t.Helper()
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, closeNow)
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	return ent.NextChargeAt
}

// The next charge is a dated invoice's, else the one that follows the next period to
// close, and none while nothing can be charged.
func TestNextChargeIsWhenMoneyNextMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	chargeOf := func(end time.Time) time.Time { return end.Add(grace).AddDate(0, 0, corebilling.ChargeNoticeDays) }

	t.Run("no live payment method", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, periodStart, day(time.August, 20))
		seedDue(t, f, periodStart, 10_000, closeNow)
		if got := nextCharge(t, f); !got.IsZero() {
			t.Errorf("next charge = %s, want none", got)
		}
	})
	t.Run("a dated invoice", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, periodStart, time.Time{})
		seedDue(t, f, day(time.June, 10), 10_000, day(time.September, 20))
		updateInvoice(t, f, seedDue(t, f, day(time.July, 10), 10_000, day(time.September, 17)), "status = 'failed'")
		updateInvoice(t, f, seedDue(t, f, periodStart, 10_000, day(time.September, 13)), "status = 'charging'")
		if got := nextCharge(t, f); !got.Equal(day(time.September, 17)) {
			t.Errorf("next charge = %s, want the failed invoice's retry, not a charge in flight", got)
		}
	})
	t.Run("the previous period not yet closed", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, periodStart, time.Time{})
		if got, want := nextCharge(t, f), chargeOf(periodEnd); !got.Equal(want) {
			t.Errorf("next charge = %s, want the previous period's at %s", got, want)
		}
	})
	t.Run("the previous period closed", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, periodStart, time.Time{})
		insertInvoice(t, f, "paid", periodStart, periodEnd, 10_000)
		if got, want := nextCharge(t, f), chargeOf(day(time.October, 10)); !got.Equal(want) {
			t.Errorf("next charge = %s, want the current period's at %s", got, want)
		}
	})
	t.Run("a payment method added this period", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, day(time.September, 11), time.Time{})
		if got, want := nextCharge(t, f), chargeOf(day(time.October, 10)); !got.Equal(want) {
			t.Errorf("next charge = %s, want the current period's at %s", got, want)
		}
	})
}

func upcomingInvoice(t *testing.T, f *fixture) corebilling.UpcomingInvoice {
	t.Helper()
	got, err := f.svc.GetUpcomingInvoice(t.Context(), f.orgID, closeNow)
	if err != nil {
		t.Fatalf("GetUpcomingInvoice: %v", err)
	}
	return got
}

// seedPeriodUsage stores the meter's count for the period starting at start.
func seedPeriodUsage(t *testing.T, f *fixture, start time.Time, events int64, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_periods (event_count, org_id, period_end, period_start, usage_computed_at)
		 values ($1, $2, $3, $4, $5)`,
		events, f.orgID, start.AddDate(0, 1, 0), start, at); err != nil {
		t.Fatalf("seed period usage: %v", err)
	}
}

// The estimate carries the meter's three states through, so a period it has not counted
// is never priced at $0, and prices a counted one as its invoice will be.
func TestUpcomingInvoicePricesOnlyACountedPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, periodStart, time.Time{})
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, closeNow, 1_170_000)
	insertInvoice(t, f, "deferred", day(time.June, 10), day(time.July, 10), 250)
	insertInvoice(t, f, "deferred", day(time.July, 10), day(time.August, 10), 120)
	insertInvoice(t, f, "waived", periodStart, periodEnd, 900)
	carrier := seedDue(t, f, day(time.May, 10), 700, closeNow)
	updateInvoice(t, f, seedDue(t, f, day(time.April, 10), 400, closeNow),
		fmt.Sprintf("status = 'deferred', covered_by = '%s'", carrier))

	never := upcomingInvoice(t, f)
	if never.Usage.Counted || !never.Usage.UsageComputedAt.IsZero() || never.Quote.TotalCents != 0 || never.Quote.Lines != nil {
		t.Errorf("never metered = %+v, want nothing priced", never)
	}
	if never.CarriedCents != 370 {
		t.Errorf("carried = %d, want the two uncovered deferred rows' 370", never.CarriedCents)
	}

	seedPeriodUsage(t, f, periodStart, 5_000_000, closeNow.Add(-72*time.Hour))
	if reaching := upcomingInvoice(t, f); reaching.Usage.Counted || reaching.Usage.UsageComputedAt.IsZero() ||
		reaching.Quote.TotalCents != 0 {
		t.Errorf("before the meter reaches the period = %+v, want its stamp and nothing priced", reaching)
	}

	seedPeriodUsage(t, f, periodEnd, 2_340_000, closeNow.Add(-time.Hour))
	counted := upcomingInvoice(t, f)
	want := corebilling.Price(currentCard(), 2_340_000)
	if !counted.Usage.Counted || counted.Quote.TotalCents != want.TotalCents || len(counted.Quote.Lines) != len(want.Lines) {
		t.Errorf("counted = %+v, want %d cents over %d lines", counted, want.TotalCents, len(want.Lines))
	}
}

func TestUpcomingInvoicePricesADealOnItsTerms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	setDealTerms(t, f, periodStart, time.Time{})
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, closeNow, 3_600_000)
	seedPeriodUsage(t, f, periodEnd, 7_200_000, closeNow)

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, closeNow)
	if err != nil || ent.Terms == nil {
		t.Fatalf("GetEntitlement = %+v, %v, want a deal", ent, err)
	}
	if got, want := upcomingInvoice(t, f).Quote, corebilling.PriceCustom(*ent.Terms, 7_200_000); got.TotalCents != want.TotalCents {
		t.Errorf("estimate = %d cents, want the deal's %d", got.TotalCents, want.TotalCents)
	}
}

func TestUpcomingInvoiceRefusesWhatCannotBePriced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	off, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := off.GetUpcomingInvoice(t.Context(), f.orgID, closeNow); !errors.Is(err, corebilling.ErrBillingDisabled) {
		t.Errorf("with billing off = %v, want ErrBillingDisabled", err)
	}
	if _, err := f.svc.GetUpcomingInvoice(t.Context(), xid.New().String(), closeNow); !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Errorf("for an unknown org = %v, want ErrOrgNotFound", err)
	}

	// A card dropped from the catalog: GetBillingStatus still reads, this cannot price.
	seedPeriodUsage(t, f, periodEnd, 1_000, closeNow)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug) values ($1, 'usage-2019-01-1')`,
		f.orgID); err != nil {
		t.Fatalf("store a dropped slug: %v", err)
	}
	if _, err := f.svc.GetUpcomingInvoice(t.Context(), f.orgID, closeNow); !errors.Is(err, corebilling.ErrPlanUnpriceable) {
		t.Errorf("on a card the catalog dropped = %v, want ErrPlanUnpriceable", err)
	}
}

func listInvoices(t *testing.T, f *fixture) []corebilling.Invoice {
	t.Helper()
	got, err := f.svc.ListInvoices(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	return got
}

// The ledger reads newest first, one org's only, and dates a charge only where one is
// still to come.
func TestListInvoicesReadsTheLedgerNewestFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedDaily(t, f, seedProjectFor(t, f), periodStart, periodEnd, 100_000)
	seedMandate(t, f, periodStart, time.Time{})
	stampMeter(t, f, closeNow)
	closePeriods(t, f, closeNow)

	paid := seedDue(t, f, day(time.July, 10), 1_000, closeNow)
	updateInvoice(t, f, paid, `status = 'paid', paid_at = now(), tax_cents = 90, provider_invoice_url = 'https://pay.example/r/1'`)
	updateInvoice(t, f, seedDue(t, f, day(time.June, 10), 1_000, closeNow), "status = 'charging'")
	updateInvoice(t, f, seedDue(t, f, day(time.May, 10), 1_000, closeNow), "status = 'void'")
	other := *f
	var err error
	if other.orgID, err = dbwriteOrg(t, f.pg); err != nil {
		t.Fatalf("create org: %v", err)
	}
	seedDue(t, &other, periodStart, 1_000, closeNow)

	got := listInvoices(t, f)
	if len(got) != 4 {
		t.Fatalf("invoices = %d, want this org's 4", len(got))
	}
	for i, want := range []corebilling.InvoiceStatus{
		corebilling.InvoiceOpen, corebilling.InvoicePaid, corebilling.InvoiceCharging, corebilling.InvoiceVoid,
	} {
		if got[i].Status != want {
			t.Errorf("invoice %d = %s, want %s newest first", i, got[i].Status, want)
		}
	}

	closed := got[0]
	if !closed.NextAttemptAt.Equal(closeNow.AddDate(0, 0, corebilling.ChargeNoticeDays)) || closed.TaxCents != nil {
		t.Errorf("open invoice = next %s, tax %v, want its charge dated and no tax yet", closed.NextAttemptAt, closed.TaxCents)
	}
	var sum int64
	for _, l := range closed.Lines {
		sum += l.AmountCents
	}
	if len(closed.Lines) == 0 || sum != closed.UsageCents {
		t.Errorf("lines = %+v, want them to sum to the period's %d", closed.Lines, closed.UsageCents)
	}
	if p := got[1]; p.TaxCents == nil || *p.TaxCents != 90 || p.PaidAt.IsZero() ||
		p.ProviderInvoiceURL != "https://pay.example/r/1" || !p.NextAttemptAt.IsZero() {
		t.Errorf("paid invoice = %+v, want its tax, receipt and payment date, and no charge to come", p)
	}
	if !got[2].NextAttemptAt.IsZero() {
		t.Errorf("charging invoice next attempt = %s, want none while a charge is in flight", got[2].NextAttemptAt)
	}
}

// A row this build cannot read fails the read, rather than a history one row short.
func TestListInvoicesRefusesARowItCannotDecode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, set := range map[string]string{
		"lines":  `lines = '{"events": 1}'`,
		"status": `status = 'disputed'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			// A status a later build added, which this build's constraint would refuse.
			if _, err := f.pg.PgW.Exec(t.Context(),
				`alter table billing_invoices drop constraint billing_invoices_status_check`); err != nil {
				t.Fatalf("drop the status check: %v", err)
			}
			seedDue(t, f, periodStart, 1_000, closeNow)
			updateInvoice(t, f, seedDue(t, f, day(time.July, 10), 1_000, closeNow), set)
			if _, err := f.svc.ListInvoices(t.Context(), f.orgID); !errors.Is(err, corebilling.ErrInvoiceUndecodable) {
				t.Errorf("ListInvoices = %v, want ErrInvoiceUndecodable", err)
			}
		})
	}
}

// The column's vocabulary and the Go one are written out separately; a status added to
// only one of them fails every ledger read for that org.
func TestInvoiceStatusesMatchTheColumnsCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	want := make([]string, 0, len(corebilling.AllInvoiceStatuses()))
	for _, s := range corebilling.AllInvoiceStatuses() {
		want = append(want, string(s))
	}
	slices.Sort(want)

	for _, name := range []string{"billing_invoices_status_check", "billing_invoice_events_to_status_check"} {
		var def string
		if err := f.pg.PgW.QueryRow(t.Context(),
			`select pg_get_constraintdef(oid) from pg_constraint where conname = $1`, name).Scan(&def); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var got []string
		for _, m := range regexp.MustCompile(`'([a-z_]+)'::text`).FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s permits %v, this build knows %v", name, got, want)
		}
	}
}

// Invariant 4, end to end: the number the dashboard shows is the number the close
// charges, including the clipping a mid-period card puts on the billable days.
func TestUpcomingInvoiceAgreesWithTheCloseItPredicts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	cardAdded, closeAfter := day(time.September, 11), day(time.October, 14)
	seedMandate(t, f, cardAdded, time.Time{})
	// Only the days before the estimate, so the close covers the same events it did.
	seedDaily(t, f, seedProjectFor(t, f), periodEnd, closeNow, 1_170_000)
	seedPeriodUsage(t, f, periodEnd, 2_340_000, closeNow)

	estimate := upcomingInvoice(t, f)
	if estimate.Quote.Events != 1_170_000 {
		t.Fatalf("estimate events = %d, want only the day the card was live", estimate.Quote.Events)
	}

	stampMeter(t, f, closeAfter)
	closePeriods(t, f, closeAfter)
	got := invoices(t, f)
	if len(got) != 1 {
		t.Fatalf("invoices = %d, want the one close", len(got))
	}
	if inv := got[0]; !inv.billedFrom.Equal(cardAdded) || inv.events != estimate.Quote.Events ||
		inv.usage != estimate.Quote.TotalCents || inv.amount != estimate.Quote.TotalCents+estimate.CarriedCents {
		t.Errorf("invoice = from %s, %d events, %d usage, %d amount; estimate said %d events, %d usage, %d amount",
			inv.billedFrom.Format(time.DateOnly), inv.events, inv.usage, inv.amount,
			estimate.Quote.Events, estimate.Quote.TotalCents, estimate.Quote.TotalCents+estimate.CarriedCents)
	}
}
