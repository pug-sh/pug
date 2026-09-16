package billing_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// The fixture org was created on 2025-03-10, so its periods run from the 10th.
var (
	grace       = 48 * time.Hour
	closeNow    = time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	periodStart = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
)

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

type invoiceRow struct {
	id, planSlug, status   string
	billedFrom, billedTo   time.Time
	events, amount, usage  int64
	nextAttemptAt          *time.Time
	lines, pricing         []byte
	periodStart, periodEnd time.Time
}

func invoices(t *testing.T, f *fixture) []invoiceRow {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select id, plan_slug, status, billed_from, billed_to, event_count, amount_cents, usage_cents,
		        next_attempt_at, lines, pricing, period_start, period_end
		 from billing_invoices where org_id = $1 order by billed_from`, f.orgID)
	if err != nil {
		t.Fatalf("query invoices: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (invoiceRow, error) {
		var r invoiceRow
		err := row.Scan(&r.id, &r.planSlug, &r.status, &r.billedFrom, &r.billedTo, &r.events, &r.amount,
			&r.usage, &r.nextAttemptAt, &r.lines, &r.pricing, &r.periodStart, &r.periodEnd)
		return r, err
	})
	if err != nil {
		t.Fatalf("collect invoices: %v", err)
	}
	return out
}

func windows(rows []invoiceRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "[%s, %s) %s; ", r.billedFrom.Format(time.DateOnly), r.billedTo.Format(time.DateOnly), r.status)
	}
	return b.String()
}

func seedProjectFor(t *testing.T, f *fixture) string {
	t.Helper()
	id := xid.New().String()
	if _, err := dbwrite.New(f.pg.PgW).CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: id, OrgID: f.orgID, DisplayName: "project-" + id,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return id
}

// seedDaily stores the same count on every day in [from, to).
func seedDaily(t *testing.T, f *fixture, projectID string, from, to time.Time, perDay int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_daily (day, event_count, org_id, project_id)
		 select d::date, $1, $2, $3 from generate_series($4::date, $5::date - 1, interval '1 day') d`,
		perDay, f.orgID, projectID, from, to); err != nil {
		t.Fatalf("seed usage_daily: %v", err)
	}
}

func stampMeter(t *testing.T, f *fixture, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_periods (org_id, period_start, period_end, usage_computed_at)
		 values ($1, $2, $3, $4)
		 on conflict (org_id, period_start) do update set usage_computed_at = excluded.usage_computed_at`,
		f.orgID, periodEnd, periodEnd.AddDate(0, 1, 0), at); err != nil {
		t.Fatalf("stamp meter: %v", err)
	}
}

// seedMandate stores a mandate added at created, ended at ended when non-zero.
func seedMandate(t *testing.T, f *fixture, created, ended time.Time) {
	t.Helper()
	status, endedAt := "active", any(nil)
	if !ended.IsZero() {
		status, endedAt = "cancelled", ended
	}
	subID := "sub_" + xid.New().String()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   create_time, currency, ended_at, id, on_demand, org_id, plan_slug, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ($1, 'USD', $2, $3, true, $4, $5, 'fake', 'cus_1', $6, $7, $1, $6)`,
		created, endedAt, xid.New().String(), f.orgID, currentCard().Slug, status, subID); err != nil {
		t.Fatalf("seed mandate: %v", err)
	}
}

func closePeriods(t *testing.T, f *fixture, now time.Time) corebilling.CloseReport {
	t.Helper()
	r, err := f.svc.ClosePeriods(t.Context(), now, grace)
	if err != nil {
		t.Fatalf("ClosePeriods: %v", err)
	}
	return r
}

// A card added mid-period is billed from the day it was added, priced on the card,
// and charged only after the notice window.
func TestCloseBillsAMandateFromTheDayItWasAdded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, day(time.August, 20).Add(15*time.Hour), time.Time{})
	stampMeter(t, f, closeNow.Add(-time.Hour))

	r := closePeriods(t, f, closeNow)
	if r.Closed != 1 || r.AwaitingCard != 0 {
		t.Fatalf("report = %+v, want one close with a card", r)
	}
	got := invoices(t, f)
	if len(got) != 1 {
		t.Fatalf("invoices = %d, want 1", len(got))
	}
	inv := got[0]
	if !inv.billedFrom.Equal(day(time.August, 20)) || !inv.billedTo.Equal(periodEnd) {
		t.Errorf("billed [%s, %s), want [08-20, 09-10)", inv.billedFrom, inv.billedTo)
	}
	if !inv.periodStart.Equal(periodStart) || !inv.periodEnd.Equal(periodEnd) {
		t.Errorf("period [%s, %s), want the anniversary window", inv.periodStart, inv.periodEnd)
	}
	want := corebilling.Price(currentCard(), 21*100_000)
	if inv.events != 2_100_000 || inv.amount != want.TotalCents || inv.usage != want.TotalCents {
		t.Errorf("events/amount/usage = %d/%d/%d, want 2100000/%d/%d",
			inv.events, inv.amount, inv.usage, want.TotalCents, want.TotalCents)
	}
	if inv.status != string(corebilling.InvoiceOpen) || inv.planSlug != currentCard().Slug {
		t.Errorf("status/slug = %s/%s, want open on the current card", inv.status, inv.planSlug)
	}
	if inv.nextAttemptAt == nil || !inv.nextAttemptAt.Equal(closeNow.AddDate(0, 0, corebilling.ChargeNoticeDays)) {
		t.Errorf("next_attempt_at = %v, want the close plus the notice window", inv.nextAttemptAt)
	}

	var lines []corebilling.Line
	if err := json.Unmarshal(inv.lines, &lines); err != nil {
		t.Fatalf("decode lines: %v", err)
	}
	var sum int64
	for _, l := range lines {
		sum += l.AmountCents
	}
	if sum != inv.amount {
		t.Errorf("lines sum to %d, invoice bills %d", sum, inv.amount)
	}
	var pricing corebilling.Pricing
	if err := json.Unmarshal(inv.pricing, &pricing); err != nil {
		t.Fatalf("decode pricing: %v", err)
	}
	if pricing.Card == nil || pricing.Terms != nil || pricing.Card.Slug != currentCard().Slug {
		t.Errorf("pricing = %s, want a snapshot of the current card", inv.pricing)
	}

	var events int
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select count(*) from billing_invoice_events where invoice_id = $1 and to_status = 'open'`,
		inv.id).Scan(&events); err != nil {
		t.Fatalf("count invoice events: %v", err)
	}
	if events != 1 {
		t.Errorf("invoice events = %d, want the close recorded once", events)
	}

	// A second pass finds billing already at the period's end.
	if r := closePeriods(t, f, closeNow.Add(time.Hour)); r != (corebilling.CloseReport{}) {
		t.Errorf("second pass report = %+v, want nothing", r)
	}
}

// The last invoice covers only the days before the card was removed.
func TestCloseStopsAtTheCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, day(time.August, 12), day(time.August, 25).Add(8*time.Hour))
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	got := invoices(t, f)
	if len(got) != 1 {
		t.Fatalf("invoices = %s, want 1", windows(got))
	}
	if !got[0].billedFrom.Equal(day(time.August, 12)) || !got[0].billedTo.Equal(day(time.August, 25)) {
		t.Errorf("billed %s, want [08-12, 08-25)", windows(got))
	}
}

// A card removed and added back bills each mandate's days once, and the gap none.
func TestCloseBillsEachMandateOnceAndTheGapNever(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 1_000_000)
	seedMandate(t, f, periodStart, day(time.August, 15))
	seedMandate(t, f, day(time.August, 20), time.Time{})
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r.Closed != 2 {
		t.Fatalf("report = %+v, want two closes", r)
	}
	got := invoices(t, f)
	if len(got) != 2 ||
		!got[0].billedFrom.Equal(periodStart) || !got[0].billedTo.Equal(day(time.August, 15)) ||
		!got[1].billedFrom.Equal(day(time.August, 20)) || !got[1].billedTo.Equal(periodEnd) {
		t.Fatalf("invoices = %s, want [08-10, 08-15) and [08-20, 09-10)", windows(got))
	}
	if got[0].events != 5_000_000 || got[1].events != 21_000_000 {
		t.Errorf("events = %d and %d, want 5M and 21M", got[0].events, got[1].events)
	}
}

// Trial days are never billed.
func TestCloseSkipsTheTrial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	testutil.SetOrgCreateTime(t, f.pg.PgW, f.orgID, time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC))
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, day(time.August, 11), time.Time{})
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	got := invoices(t, f)
	// The trial ends 08-24 09:00, so its last partial day is not billed either.
	if len(got) != 1 || !got[0].billedFrom.Equal(day(time.August, 25)) {
		t.Fatalf("invoices = %s, want one billed from 08-25", windows(got))
	}
}

// A deal recorded before any card is invoiced from its terms, and reported as
// waiting on one (§19.15).
func TestCloseInvoicesADealWithNoCardFromItsTerms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	change := setDeal()
	change.RateCentsPerMillion = new(int64(3_000))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set terms_effective_at = $1 where org_id = $2`,
		day(time.August, 30).Add(10*time.Hour), f.orgID); err != nil {
		t.Fatalf("backdate terms: %v", err)
	}
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, day(time.May, 1), periodEnd, 1_000_000)
	stampMeter(t, f, closeNow)

	r := closePeriods(t, f, closeNow)
	if r.Closed != 1 || r.AwaitingCard != 1 {
		t.Fatalf("report = %+v, want one close waiting on a card", r)
	}
	got := invoices(t, f)
	if len(got) != 1 {
		t.Fatalf("invoices = %d, want only the period the terms reached", len(got))
	}
	inv := got[0]
	want := corebilling.PriceCustom(corebilling.CustomTerms{FlatFeeCents: 40_000, RateCentsPerMillion: 3_000}, 11_000_000)
	if inv.planSlug != corebilling.SlugCustom || !inv.billedFrom.Equal(day(time.August, 30)) ||
		inv.events != 11_000_000 || inv.amount != want.TotalCents {
		t.Errorf("invoice = %s from %s, %d events for %d, want custom from 08-30, 11M for %d",
			inv.planSlug, inv.billedFrom, inv.events, inv.amount, want.TotalCents)
	}
	var pricing corebilling.Pricing
	if err := json.Unmarshal(inv.pricing, &pricing); err != nil {
		t.Fatalf("decode pricing: %v", err)
	}
	if pricing.Terms == nil || pricing.Card != nil {
		t.Errorf("pricing = %s, want a snapshot of the terms", inv.pricing)
	}
}

// A stalled meter delays an invoice, never mis-bills one.
func TestCloseHoldsUntilTheMeterIsFinal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.August, 11), time.Time{})

	if r := closePeriods(t, f, closeNow); r.Held != 1 {
		t.Errorf("never metered: report = %+v, want held", r)
	}
	stampMeter(t, f, periodEnd.Add(grace).Add(-time.Second))
	if r := closePeriods(t, f, closeNow); r.Held != 1 {
		t.Errorf("stamped inside the grace: report = %+v, want held", r)
	}
	if n := len(invoices(t, f)); n != 0 {
		t.Fatalf("invoices = %d while held, want 0", n)
	}

	stampMeter(t, f, periodEnd.Add(grace))
	if r := closePeriods(t, f, closeNow); r.Held != 0 || r.Waived != 1 {
		t.Errorf("final meter: report = %+v, want the period closed", r)
	}
}

// Nothing closes before period_end + grace, however fresh the stamp.
func TestCloseWaitsOutTheGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.September, 1), time.Time{})
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, periodEnd.Add(grace).Add(-time.Minute)); r != (corebilling.CloseReport{}) {
		t.Errorf("report = %+v, want nothing before the grace ends", r)
	}
}

// Nothing to bill is waived, and never scheduled for a charge.
func TestCloseWaivesAPeriodWithNothingToBill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 1_000)
	seedMandate(t, f, day(time.August, 12), time.Time{})
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r.Waived != 1 || r.Closed != 0 {
		t.Fatalf("report = %+v, want one waived", r)
	}
	got := invoices(t, f)
	if len(got) != 1 || got[0].status != string(corebilling.InvoiceWaived) || got[0].nextAttemptAt != nil {
		t.Errorf("invoices = %s, want waived with no charge date", windows(got))
	}
}

// An anchor moved after a close starts the next one where the last ended.
func TestCloseAfterAnAnchorChangeBillsNoDayTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, day(time.October, 20), 100_000)
	seedMandate(t, f, day(time.August, 11), time.Time{})
	stampMeter(t, f, closeNow)
	closePeriods(t, f, closeNow)

	change := pinCard()
	change.AnchorDay = new(1)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	later := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	stampMeter(t, f, later)
	closePeriods(t, f, later)

	got := invoices(t, f)
	if len(got) != 2 {
		t.Fatalf("invoices = %s, want two", windows(got))
	}
	if !got[1].billedFrom.Equal(got[0].billedTo) || !got[1].billedTo.Equal(day(time.October, 1)) {
		t.Errorf("second invoice [%s, %s), want [%s, 10-01)", got[1].billedFrom, got[1].billedTo, got[0].billedTo)
	}
}

// A pass behind by three periods closes them all; the fourth is lost and says so.
func TestCloseCatchesUpAndReportsWhatFellOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, day(time.May, 10), periodEnd, 100_000)
	seedMandate(t, f, day(time.May, 10), time.Time{})
	stampMeter(t, f, closeNow)

	r := closePeriods(t, f, closeNow)
	if r.Closed != 3 || r.Dropped != 1 {
		t.Fatalf("report = %+v, want three closed and one dropped", r)
	}
	got := invoices(t, f)
	if len(got) != 3 || !got[0].billedFrom.Equal(day(time.June, 10)) {
		t.Fatalf("invoices = %s, want three from 06-10", windows(got))
	}

	// The loss is reported for a day, not every hour until the next anniversary.
	if r := closePeriods(t, f, closeNow.Add(2*24*time.Hour)); r.Dropped != 0 {
		t.Errorf("a day later report = %+v, want the drop no longer reported", r)
	}
}

// The unique index, not the lock, keeps two passes from closing one window.
func TestCloseSkipsAWindowAnotherPassClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.August, 20), time.Time{})
	stampMeter(t, f, closeNow)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, currency, event_count, id, lines, org_id,
		   period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
		 values (0, '2026-08-20', '2026-08-21', 'USD', 0, $1, '[]', $2, $3, $4, 'x', '{}', 'waived', 0, now())`,
		xid.New().String(), f.orgID, periodEnd, periodStart); err != nil {
		t.Fatalf("pre-insert: %v", err)
	}
	// Billing has reached 08-21, so the close starts there rather than colliding.
	closePeriods(t, f, closeNow)
	if got := invoices(t, f); len(got) != 2 || !got[1].billedFrom.Equal(day(time.August, 21)) {
		t.Errorf("invoices = %s, want the close to start where the first ended", windows(got))
	}
}

// Free orgs, orgs with billing off and orgs pug cannot read write nothing.
func TestCloseWritesNothingItShouldNot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Run("a free org", func(t *testing.T) {
		f := newFixture(t)
		project := seedProjectFor(t, f)
		seedDaily(t, f, project, periodStart, periodEnd, 1_000_000)
		stampMeter(t, f, closeNow)
		if r := closePeriods(t, f, closeNow); r != (corebilling.CloseReport{}) || len(invoices(t, f)) != 0 {
			t.Errorf("report = %+v, want no invoice for an org with no mandate or deal", r)
		}
	})

	t.Run("billing off", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, day(time.January, 1), time.Time{})
		stampMeter(t, f, closeNow)
		off, err := corebilling.NewService(f.pg.PgRO, f.pg.PgW, false, nil)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		if _, err := off.ClosePeriods(t.Context(), closeNow, grace); err != nil {
			t.Fatalf("ClosePeriods: %v", err)
		}
		if n := len(invoices(t, f)); n != 0 {
			t.Errorf("invoices = %d with billing off, want 0", n)
		}
	})

	t.Run("an unknown subscription status", func(t *testing.T) {
		f := newFixture(t)
		seedMandate(t, f, day(time.January, 1), time.Time{})
		stampMeter(t, f, closeNow)
		if _, err := f.pg.PgW.Exec(t.Context(),
			`update billing_subscriptions set status = 'mystery' where org_id = $1`, f.orgID); err != nil {
			t.Fatalf("corrupt status: %v", err)
		}
		r, err := f.svc.ClosePeriods(t.Context(), closeNow, grace)
		if err != nil || r.Undecodable != 1 {
			t.Errorf("report = %+v, err = %v, want the org counted undecodable", r, err)
		}
		if errors.Is(err, corebilling.ErrSubscriptionUndecodable) {
			t.Error("an undecodable org failed the whole step")
		}
	})
}

// A replacement card added before the old one's cancellation landed bills the
// overlap once.
func TestCloseBillsOverlappingMandatesOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, periodStart, day(time.August, 25))
	seedMandate(t, f, day(time.August, 20), time.Time{})
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	got := invoices(t, f)
	if len(got) != 2 || !got[0].billedTo.Equal(day(time.August, 25)) || !got[1].billedFrom.Equal(day(time.August, 25)) {
		t.Errorf("invoices = %s, want [08-10, 08-25) and [08-25, 09-10)", windows(got))
	}
}

// A lost period nothing later will move past is reported for a day, not every
// hour until the next anniversary.
func TestCloseReportsADroppedPeriodForADay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.May, 12), day(time.June, 5))
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r.Dropped != 1 {
		t.Errorf("report = %+v, want the lost period dropped", r)
	}
	if r := closePeriods(t, f, closeNow.Add(2*24*time.Hour)); r.Dropped != 0 {
		t.Errorf("two days later report = %+v, want it no longer reported", r)
	}
}

// closeMonths closes n consecutive periods from 2025-09-10, each with events on
// its first day, under a mandate added that day.
func closeMonths(t *testing.T, f *fixture, events ...int64) []corebilling.CloseReport {
	t.Helper()
	project := seedProjectFor(t, f)
	first := time.Date(2025, 9, 10, 0, 0, 0, 0, time.UTC)
	seedMandate(t, f, first, time.Time{})
	var out []corebilling.CloseReport
	for i, n := range events {
		start := first.AddDate(0, i, 0)
		if n > 0 {
			seedDaily(t, f, project, start, start.AddDate(0, 0, 1), n)
		}
		now := start.AddDate(0, 1, 0).Add(grace).Add(6 * time.Hour)
		stampMeter(t, f, now)
		out = append(out, closePeriods(t, f, now))
	}
	return out
}

type carriedRow struct {
	status               string
	usage, carried, owed int64
	coveredBy            *string
	id                   string
}

func carriedRows(t *testing.T, f *fixture) []carriedRow {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select id, status, usage_cents, carried_cents, amount_cents, covered_by
		 from billing_invoices where org_id = $1 order by billed_from`, f.orgID)
	if err != nil {
		t.Fatalf("query invoices: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (carriedRow, error) {
		var r carriedRow
		err := row.Scan(&r.id, &r.status, &r.usage, &r.carried, &r.owed, &r.coveredBy)
		return r, err
	})
	if err != nil {
		t.Fatalf("collect invoices: %v", err)
	}
	return out
}

// $2.50 a month is charged $5.00 every second month.
func TestCloseDefersASmallInvoiceAndCarriesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	reports := closeMonths(t, f, 162_500, 162_500)
	if reports[0].Deferred != 1 || reports[1].Closed != 1 {
		t.Fatalf("reports = %+v, want deferred then charged", reports)
	}
	got := carriedRows(t, f)
	if len(got) != 2 {
		t.Fatalf("invoices = %+v, want 2", got)
	}
	if got[0].status != "deferred" || got[0].coveredBy == nil || *got[0].coveredBy != got[1].id {
		t.Errorf("first = %+v, want deferred and covered by the second", got[0])
	}
	if got[1].status != "open" || got[1].usage != 250 || got[1].carried != 250 || got[1].owed != 500 {
		t.Errorf("second = %+v, want open owing 250 + 250 carried", got[1])
	}
}

// A month with nothing to bill waits; it neither charges nor sweeps the balance.
func TestCloseWithNothingToBillLeavesTheBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	closeMonths(t, f, 162_500, 0)
	got := carriedRows(t, f)
	if len(got) != 2 || got[0].status != "deferred" || got[0].coveredBy != nil ||
		got[1].status != "waived" || got[1].carried != 0 {
		t.Errorf("invoices = %+v, want the balance uncovered beside a waived month", got)
	}
}

// Eleven deferred periods and the twelfth sweeps: charged at a dollar, written off
// under it.
func TestCloseSweepsABalanceAtTheTwelfthPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	twelve := func(n int64) []int64 {
		out := make([]int64, corebilling.MaxDeferPeriods)
		for i := range out {
			out[i] = n
		}
		return out
	}

	t.Run("over a dollar is charged", func(t *testing.T) {
		f := newFixture(t)
		reports := closeMonths(t, f, twelve(103_750)...)
		if last := reports[len(reports)-1]; last.Swept != 1 || last.Closed != 1 {
			t.Fatalf("last report = %+v, want a charged sweep", last)
		}
		got := carriedRows(t, f)
		sweep := got[len(got)-1]
		if sweep.status != "open" || sweep.carried != 11*15 || sweep.owed != 12*15 {
			t.Errorf("sweep = %+v, want open owing 180 with 165 carried", sweep)
		}
		for _, row := range got[:len(got)-1] {
			if row.status != "deferred" || row.coveredBy == nil || *row.coveredBy != sweep.id {
				t.Errorf("row = %+v, want deferred and covered by the sweep", row)
			}
		}
	})

	t.Run("under a dollar is written off", func(t *testing.T) {
		f := newFixture(t)
		reports := closeMonths(t, f, twelve(100_200)...)
		for i, r := range reports[:len(reports)-1] {
			if r.Deferred != 1 || r.Swept != 0 {
				t.Fatalf("report %d = %+v, want deferred", i, r)
			}
		}
		if last := reports[len(reports)-1]; last.Swept != 1 || last.Waived != 1 {
			t.Fatalf("last report = %+v, want a waived sweep", last)
		}
		for _, row := range carriedRows(t, f) {
			if row.status != "waived" || row.coveredBy != nil || row.carried != 0 {
				t.Errorf("row = %+v, want every period waived", row)
			}
		}
	})
}
