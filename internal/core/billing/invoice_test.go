package billing_test

import (
	"encoding/json"
	"fmt"
	"slices"
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
	id, planSlug, status           string
	billedFrom, billedTo           time.Time
	events, amount, usage, carried int64
	coveredBy                      *string
	nextAttemptAt                  *time.Time
	lines, pricing                 []byte
	periodStart, periodEnd         time.Time
}

func invoices(t *testing.T, f *fixture) []invoiceRow {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		`select id, plan_slug, status, billed_from, billed_to, event_count, amount_cents, usage_cents,
		        carried_cents, covered_by, next_attempt_at, lines, pricing, period_start, period_end
		 from billing_invoices where org_id = $1 order by billed_from`, f.orgID)
	if err != nil {
		t.Fatalf("query invoices: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (invoiceRow, error) {
		var r invoiceRow
		err := row.Scan(&r.id, &r.planSlug, &r.status, &r.billedFrom, &r.billedTo, &r.events, &r.amount,
			&r.usage, &r.carried, &r.coveredBy, &r.nextAttemptAt, &r.lines, &r.pricing, &r.periodStart, &r.periodEnd)
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

// seedMandate stores a mandate added at created, ended at ended when non-zero, and
// returns its provider id.
func seedMandate(t *testing.T, f *fixture, created, ended time.Time) string {
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
	return subID
}

// insertInvoice stores a row as an earlier close left it.
func insertInvoice(t *testing.T, f *fixture, status string, from, to time.Time, cents int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, currency, event_count, id, lines, org_id,
		   period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
		 values ($1, $2, $3, 'USD', 0, $4, '[]', $5, $6, $7, 'x', '{}', $8, $1, now())`,
		cents, from, to, xid.New().String(), f.orgID, to, from, status); err != nil {
		t.Fatalf("insert invoice: %v", err)
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

// A card removed and added back bills both mandates' days on one invoice, with one
// allowance, and the gap none.
func TestCloseBillsEveryMandateOnOneInvoiceAndTheGapNever(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 1_000_000)
	seedMandate(t, f, periodStart, day(time.August, 15))
	seedMandate(t, f, day(time.August, 20), time.Time{})
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r.Closed != 1 {
		t.Fatalf("report = %+v, want one close", r)
	}
	got := invoices(t, f)
	if len(got) != 1 || !got[0].billedFrom.Equal(periodStart) || !got[0].billedTo.Equal(periodEnd) {
		t.Fatalf("invoices = %s, want [08-10, 09-10)", windows(got))
	}
	if want := corebilling.Price(currentCard(), 26_000_000); got[0].events != 26_000_000 || got[0].usage != want.TotalCents {
		t.Errorf("events/usage = %d/%d, want 26M priced once at %d", got[0].events, got[0].usage, want.TotalCents)
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

// A deal recorded after an extended trial leaves the trial's days unbilled.
func TestCloseSkipsATrialADealEnded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, periodStart, time.Time{})
	if _, err := f.svc.ExtendTrial(t.Context(), f.orgID, actor, 10, periodStart); err != nil {
		t.Fatalf("extend the trial: %v", err)
	}
	setDealTerms(t, f, day(time.August, 30), time.Time{})
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	if got := invoices(t, f); len(got) != 2 || !got[0].billedFrom.Equal(day(time.August, 20)) {
		t.Errorf("invoices = %s, want the card billed from the trial's end", windows(got))
	}
}

// A deal recorded before any card is invoiced from its terms, and reported as
// waiting on one (§19.15).
func TestCloseInvoicesADealWithNoCardFromItsTerms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	setDealTerms(t, f, day(time.August, 30).Add(10*time.Hour), time.Time{})
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

// A pass five periods behind closes the newest three and writes off each older one
// once.
func TestCloseCatchesUpAndWritesOffWhatFellOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, day(time.April, 10), periodEnd, 100_000)
	seedMandate(t, f, day(time.April, 10), time.Time{})
	stampMeter(t, f, closeNow)

	r := closePeriods(t, f, closeNow)
	if r.Closed != 3 || r.Dropped != 2 {
		t.Fatalf("report = %+v, want three closed and two written off", r)
	}
	got := invoices(t, f)
	if len(got) != 5 || got[0].status != string(corebilling.InvoiceWaived) ||
		got[1].status != string(corebilling.InvoiceWaived) || got[1].nextAttemptAt != nil ||
		!got[2].billedFrom.Equal(day(time.June, 10)) {
		t.Fatalf("invoices = %s, want 04-10 and 05-10 waived and three from 06-10", windows(got))
	}
	if r := closePeriods(t, f, closeNow.Add(time.Hour)); r != (corebilling.CloseReport{}) {
		t.Errorf("next pass report = %+v, want the write-offs not reported again", r)
	}
}

// A window another pass already billed moves where the next close starts.
func TestCloseStartsWhereAnotherPassStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.August, 20), time.Time{})
	stampMeter(t, f, closeNow)
	insertInvoice(t, f, "waived", day(time.August, 20), day(time.August, 21), 0)

	closePeriods(t, f, closeNow)
	if got := invoices(t, f); len(got) != 2 || !got[1].billedFrom.Equal(day(time.August, 21)) {
		t.Errorf("invoices = %s, want the close to start where the first ended", windows(got))
	}
}

// Free orgs and orgs with billing off write nothing.
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
}

// A subscription with an unrecognized status bills none of its days, and holds up nothing.
func TestCloseBillsBesideAnUnknownSubscriptionStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, day(time.August, 20), time.Time{})
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set status = 'pending', ended_at = $2 where org_id = $1`,
		f.orgID, periodEnd.AddDate(1, 0, 0)); err != nil {
		t.Fatalf("leave a checkout pending: %v", err)
	}
	seedMandate(t, f, periodStart, day(time.August, 20))
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	if got := invoices(t, f); len(got) != 1 || !got[0].billedTo.Equal(day(time.August, 20)) {
		t.Errorf("invoices = %s, want [08-10, 08-20) from the cancelled mandate alone", windows(got))
	}
}

// One org's failed close does not hold up the orgs listed after it.
func TestCloseCarriesOnPastAFailingOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	other, err := dbwrite.New(f.pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "other",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, f.pg.PgW, other.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	for _, org := range []*fixture{f, {svc: f.svc, pg: f.pg, orgID: other.ID}} {
		seedDaily(t, org, seedProjectFor(t, org), periodStart, periodEnd, 100_000)
		seedMandate(t, org, periodStart, time.Time{})
		stampMeter(t, org, closeNow)
	}
	for _, stmt := range []string{
		`create function fail_first_org() returns trigger language plpgsql as $$ begin
		   if new.org_id = (select min(id) from orgs) then raise exception 'injected'; end if;
		   return new;
		 end $$`,
		`create trigger fail_first_org before insert on billing_invoices
		   for each row execute function fail_first_org()`,
	} {
		if _, err := f.pg.PgW.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("fail the first org's close: %v", err)
		}
	}

	r, err := f.svc.ClosePeriods(t.Context(), closeNow, grace)
	if err == nil || r.Closed != 1 {
		t.Errorf("report = %+v, err = %v, want the second org closed and the failure returned", r, err)
	}
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
	if len(got) != 1 || !got[0].billedFrom.Equal(periodStart) || !got[0].billedTo.Equal(periodEnd) ||
		got[0].events != 3_100_000 {
		t.Errorf("invoices = %s, want [08-10, 09-10) counting each day once", windows(got))
	}
}

// A lost period nothing later will move past is written off, so it is reported once.
func TestCloseWritesOffADroppedPeriodOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.May, 12), day(time.June, 5))
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r != (corebilling.CloseReport{Dropped: 1}) {
		t.Errorf("report = %+v, want the lost period dropped", r)
	}
	got := invoices(t, f)
	if len(got) != 1 || got[0].status != string(corebilling.InvoiceWaived) ||
		!got[0].billedFrom.Equal(day(time.May, 12)) || !got[0].billedTo.Equal(day(time.June, 5)) {
		t.Errorf("invoices = %s, want [05-12, 06-05) waived", windows(got))
	}
	var detail string
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select detail from billing_invoice_events where invoice_id = $1`, got[0].id).Scan(&detail); err != nil {
		t.Fatalf("read invoice event: %v", err)
	}
	if detail != "dropped" {
		t.Errorf("event detail = %q, want dropped", detail)
	}
	if r := closePeriods(t, f, closeNow.Add(time.Hour)); r.Dropped != 0 {
		t.Errorf("next pass report = %+v, want it not reported again", r)
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

// $2.50 a month is charged $5.00 every second month, and a carried balance is not
// carried again.
func TestCloseDefersASmallInvoiceAndCarriesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	reports := closeMonths(t, f, 162_500, 162_500, 162_500)
	if reports[0].Deferred != 1 || reports[1].Closed != 1 || reports[2].Deferred != 1 {
		t.Fatalf("reports = %+v, want deferred, charged, deferred", reports)
	}
	cents := corebilling.Price(currentCard(), 162_500).TotalCents
	got := invoices(t, f)
	if len(got) != 3 {
		t.Fatalf("invoices = %s, want 3", windows(got))
	}
	if got[0].status != "deferred" || got[0].nextAttemptAt != nil ||
		got[0].coveredBy == nil || *got[0].coveredBy != got[1].id {
		t.Errorf("first = %+v, want deferred and covered by the second", got[0])
	}
	if got[1].status != "open" || got[1].usage != cents || got[1].carried != cents || got[1].amount != 2*cents {
		t.Errorf("second = %+v, want open owing %d with %d carried", got[1], 2*cents, cents)
	}
	if got[2].status != "deferred" || got[2].carried != 0 || got[2].coveredBy != nil {
		t.Errorf("third = %+v, want deferred with nothing carried", got[2])
	}
}

// A month with nothing to bill waits; it neither charges nor sweeps the balance.
func TestCloseWithNothingToBillLeavesTheBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	closeMonths(t, f, 162_500, 0)
	got := invoices(t, f)
	if len(got) != 2 || got[0].status != "deferred" || got[0].coveredBy != nil ||
		got[1].status != "waived" || got[1].carried != 0 {
		t.Errorf("invoices = %s, want the balance uncovered beside a waived month", windows(got))
	}
}

// Eleven deferred periods and the twelfth sweeps: charged at a dollar, written off
// under it.
func TestCloseSweepsABalanceAtTheTwelfthPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Run("over a dollar is charged", func(t *testing.T) {
		f := newFixture(t)
		reports := closeMonths(t, f, slices.Repeat([]int64{103_750}, corebilling.MaxDeferPeriods)...)
		if last := reports[len(reports)-1]; last.Swept != 1 || last.Closed != 1 {
			t.Fatalf("last report = %+v, want a charged sweep", last)
		}
		each := corebilling.Price(currentCard(), 103_750).TotalCents
		got := invoices(t, f)
		sweep := got[len(got)-1]
		if sweep.status != "open" || sweep.carried != 11*each || sweep.amount != 12*each {
			t.Errorf("sweep = %+v, want open owing %d with %d carried", sweep, 12*each, 11*each)
		}
		for _, row := range got[:len(got)-1] {
			if row.status != "deferred" || row.coveredBy == nil || *row.coveredBy != sweep.id {
				t.Errorf("row = %+v, want deferred and covered by the sweep", row)
			}
		}
	})

	t.Run("under a dollar is written off", func(t *testing.T) {
		f := newFixture(t)
		reports := closeMonths(t, f, slices.Repeat([]int64{100_200}, corebilling.MaxDeferPeriods)...)
		for i, r := range reports[:len(reports)-1] {
			if r.Deferred != 1 || r.Swept != 0 {
				t.Fatalf("report %d = %+v, want deferred", i, r)
			}
		}
		if last := reports[len(reports)-1]; last.Swept != 1 || last.Waived != 1 {
			t.Fatalf("last report = %+v, want a waived sweep", last)
		}
		got := invoices(t, f)
		for _, row := range got {
			if row.status != "waived" || row.coveredBy != nil || row.carried != 0 || row.nextAttemptAt != nil {
				t.Errorf("row = %+v, want every period waived", row)
			}
		}
		var swept int
		if err := f.pg.PgRO.QueryRow(t.Context(),
			`select count(*) from billing_invoice_events
			 where from_status = 'deferred' and to_status = 'waived' and detail = $1`,
			"swept by "+got[len(got)-1].id).Scan(&swept); err != nil {
			t.Fatalf("count sweep events: %v", err)
		}
		if swept != len(got)-1 {
			t.Errorf("sweep events = %d, want one per written-off period", swept)
		}
	})
}

// setDealTerms records a deal with a fee and a rate whose terms took effect at from
// and end at until, when non-zero.
func setDealTerms(t *testing.T, f *fixture, from, until time.Time) {
	t.Helper()
	change := setDeal()
	change.RateCentsPerMillion = new(int64(3_000))
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, change); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	var ends any
	if !until.IsZero() {
		ends = until
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set terms_effective_at = $1, contract_ends_at = $2 where org_id = $3`,
		from, ends, f.orgID); err != nil {
		t.Fatalf("date the deal: %v", err)
	}
}

// A deal that ends mid-period bills its own days on its terms, and the rest on a
// card if there is one.
func TestCloseSplitsAPeriodWhereADealEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Run("with a card", func(t *testing.T) {
		f := newFixture(t)
		project := seedProjectFor(t, f)
		seedDaily(t, f, project, periodStart, periodEnd, 100_000)
		seedMandate(t, f, periodStart, time.Time{})
		setDealTerms(t, f, periodStart, day(time.August, 25))
		stampMeter(t, f, closeNow)

		if r := closePeriods(t, f, closeNow); r.Closed != 2 || r.AwaitingCard != 0 {
			t.Fatalf("report = %+v, want two closes with a card", r)
		}
		got := invoices(t, f)
		if len(got) != 2 ||
			got[0].planSlug != corebilling.SlugCustom || !got[0].billedTo.Equal(day(time.August, 25)) ||
			got[1].planSlug != currentCard().Slug || !got[1].billedFrom.Equal(day(time.August, 25)) {
			t.Errorf("invoices = %s, want the deal to 08-25 and the card after", windows(got))
		}
	})

	t.Run("without a card", func(t *testing.T) {
		f := newFixture(t)
		project := seedProjectFor(t, f)
		seedDaily(t, f, project, periodStart, periodEnd, 100_000)
		setDealTerms(t, f, periodStart, day(time.August, 25))
		stampMeter(t, f, closeNow)

		closePeriods(t, f, closeNow)
		got := invoices(t, f)
		if len(got) != 1 || got[0].planSlug != corebilling.SlugCustom ||
			!got[0].billedFrom.Equal(periodStart) || !got[0].billedTo.Equal(day(time.August, 25)) {
			t.Errorf("invoices = %s, want the deal's days [08-10, 08-25)", windows(got))
		}
	})
}

// A deal recorded mid-period over a card leaves the card's earlier days on the card.
func TestCloseBillsTheCardDaysBeforeADeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, periodStart, periodEnd, 100_000)
	seedMandate(t, f, periodStart, time.Time{})
	setDealTerms(t, f, day(time.August, 30).Add(10*time.Hour), time.Time{})
	stampMeter(t, f, closeNow)

	closePeriods(t, f, closeNow)
	got := invoices(t, f)
	if len(got) != 2 ||
		got[0].planSlug != currentCard().Slug || !got[0].billedTo.Equal(day(time.August, 30)) ||
		got[1].planSlug != corebilling.SlugCustom || !got[1].billedFrom.Equal(day(time.August, 30)) {
		t.Errorf("invoices = %s, want the card to 08-30 and the deal after", windows(got))
	}
}

// A slug no card answers to holds the invoice rather than billing it at nothing,
// and the deal days after it wait too.
func TestCloseHoldsAnUnpriceablePeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	seedMandate(t, f, day(time.August, 12), time.Time{})
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set plan_slug = 'retired_card' where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("unknown slug: %v", err)
	}
	setDealTerms(t, f, day(time.August, 30), time.Time{})
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r != (corebilling.CloseReport{Unpriceable: 1}) {
		t.Errorf("report = %+v, want unpriceable", r)
	}
	if n := len(invoices(t, f)); n != 0 {
		t.Errorf("invoices = %d, want 0", n)
	}
}

// The last close of a gone mandate sweeps: its own $2.50 is charged, not deferred
// with nothing later to carry it.
func TestCloseSweepsTheLastCloseOfAGoneMandate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	project := seedProjectFor(t, f)
	seedDaily(t, f, project, day(time.August, 12), day(time.August, 13), 162_500)
	seedMandate(t, f, day(time.August, 12), day(time.August, 13))
	stampMeter(t, f, closeNow)

	if r := closePeriods(t, f, closeNow); r.Closed != 1 || r.Deferred != 0 {
		t.Errorf("report = %+v, want the final close charged", r)
	}
	if got := invoices(t, f); len(got) != 1 || got[0].status != string(corebilling.InvoiceOpen) {
		t.Errorf("invoices = %s, want one open", windows(got))
	}
}

// A deal that lapsed with no card sweeps, since no later close carries its balance;
// one in force defers it.
func TestCloseSweepsALapsedDealWithNoCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for name, tc := range map[string]struct {
		until time.Time
		want  corebilling.InvoiceStatus
	}{
		"lapsed":   {day(time.August, 25), corebilling.InvoiceOpen},
		"in force": {time.Time{}, corebilling.InvoiceDeferred},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			project := seedProjectFor(t, f)
			seedDaily(t, f, project, periodStart, day(time.August, 11), 50_000)
			insertInvoice(t, f, "deferred", day(time.July, 10), day(time.July, 11), 250)
			setDealTerms(t, f, periodStart, tc.until)
			if _, err := f.pg.PgW.Exec(t.Context(),
				`update billing_entitlements set flat_fee_cents = null where org_id = $1`, f.orgID); err != nil {
				t.Fatalf("drop the fee: %v", err)
			}
			stampMeter(t, f, closeNow)

			closePeriods(t, f, closeNow)
			if got := invoices(t, f); len(got) != 2 || got[1].status != string(tc.want) {
				t.Errorf("invoices = %s, want the deal's close %s", windows(got), tc.want)
			}
		})
	}
}
