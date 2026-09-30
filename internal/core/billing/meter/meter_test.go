package meter_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/meter"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// recorder is a UsageMeter that keeps what it was sent and fails on demand.
type recorder struct {
	// attempts is every statement offered, sent only those that landed.
	attempts, sent []corebilling.UsageStatement
	fail           error
	// during runs inside IngestUsage, to disturb the ledger mid-statement.
	during func()
}

func (r *recorder) IngestUsage(_ context.Context, s corebilling.UsageStatement) error {
	r.attempts = append(r.attempts, s)
	if r.during != nil {
		r.during()
	}
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, s)
	return nil
}

func (r *recorder) VerifyMetering(context.Context, int, []string) error { return nil }

// last is the latest statement that landed for customer.
func (r *recorder) last(t *testing.T, customer string) corebilling.UsageStatement {
	t.Helper()
	for _, s := range slices.Backward(r.sent) {
		if s.CustomerID == customer {
			return s
		}
	}
	t.Fatalf("nothing was stated for %s", customer)
	return corebilling.UsageStatement{}
}

type fixture struct {
	pg    *testutil.TestPostgres
	org   string
	proj  string
	meter *recorder
	svc   *meter.Service
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	ents, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("entitlement service: %v", err)
	}
	rec := &recorder{}
	f := &fixture{pg: pg, meter: rec, svc: meter.NewService(pg.PgRO, pg.PgW, ents, rec, "dodo")}
	f.org, f.proj = f.newOrg(t, xid.New().String())
	return f
}

// newOrg creates an org with one project. The pass meters orgs in id order, so a
// test that needs one first picks its id.
func (f *fixture) newOrg(t *testing.T, id string) (org, proj string) {
	t.Helper()
	w := dbwrite.New(f.pg.PgW)
	if _, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: id, DisplayName: "acme"}); err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj = xid.New().String()
	if _, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{ID: proj, OrgID: id, DisplayName: "p"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return id, proj
}

func (f *fixture) usage(t *testing.T, day time.Time, n int64) { f.usageOf(t, f.proj, day, n) }

func (f *fixture) usageOf(t *testing.T, proj string, day time.Time, n int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_daily (day, event_count, org_id, project_id)
		 select $1, $2, org_id, id from projects where id = $3
		 on conflict (project_id, day) do update set event_count = excluded.event_count`,
		day, n, proj); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// usageMetered stamps the usage pass as having verified its counts at at, which
// every statement is gated on.
func (f *fixture) usageMetered(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_periods (org_id, period_start, period_end, usage_computed_at)
		 values ($1, $2, $2::timestamptz + interval '1 month', $3)
		 on conflict (org_id, period_start) do update set usage_computed_at = excluded.usage_computed_at`,
		f.org, at.Truncate(24*time.Hour), at); err != nil {
		t.Fatalf("stamp usage: %v", err)
	}
}

// run is one pass at now, the usage pass having verified its counts just before.
func (f *fixture) run(t *testing.T, now time.Time) (meter.Report, error) {
	t.Helper()
	f.usageMetered(t, now.Add(-10*time.Minute))
	return f.svc.Run(t.Context(), now)
}

// subscribe gives the org a new live subscription, ending the one it held, as a
// cutover or a re-subscription does. It returns the subscription's id.
func (f *fixture) subscribe(t *testing.T, plan, customer string, start, end time.Time) string {
	return f.subscribeOrg(t, f.org, plan, customer, start, end)
}

func (f *fixture) subscribeOrg(t *testing.T, org, plan, customer string, start, end time.Time) string {
	t.Helper()
	f.cancelOrg(t, org)
	subID := "sub_" + xid.New().String()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   plan_slug, price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ('USD', $1, $2, $3, $4, $5, 100, 'dodo', $6, 'active', $7, now(), 'active')`,
		end, start, xid.New().String(), org, plan, customer, subID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return subID
}

// renew moves the org's live subscription into its next period: the provider renews
// a subscription in place, so its id stays.
func (f *fixture) renew(t *testing.T, start, end time.Time) {
	t.Helper()
	tag, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set current_period_start = $1, current_period_end = $2
		 where org_id = $3 and status = 'active'`, start, end, f.org)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("renew: %v (%d rows)", err, tag.RowsAffected())
	}
}

func (f *fixture) cancel(t *testing.T) { f.cancelOrg(t, f.org) }

func (f *fixture) cancelOrg(t *testing.T, org string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set status = 'cancelled', provider_status = 'cancelled'
		 where org_id = $1 and status in ('active', 'past_due')`, org); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func (f *fixture) ledger(t *testing.T, start time.Time) (own, carry []int64, acked bool) {
	t.Helper()
	err := f.pg.PgW.QueryRow(t.Context(),
		`select own_events, carry_events, acked from billing_meter_periods where org_id = $1 and period_start = $2`,
		f.org, start).Scan(&own, &carry, &acked)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return own, carry, acked
}

// window is the days a period's row covers, and how far they were summed while its
// subscription was live.
func (f *fixture) window(t *testing.T, start time.Time) (from, to, summedThrough time.Time) {
	t.Helper()
	err := f.pg.PgW.QueryRow(t.Context(),
		`select window_start, window_end, summed_through from billing_meter_periods where org_id = $1 and period_start = $2`,
		f.org, start).Scan(&from, &to, &summedThrough)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	return from, to, summedThrough
}

func (f *fixture) periods(t *testing.T, org string) int {
	t.Helper()
	var n int
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select count(*) from billing_meter_periods where org_id = $1`, org).Scan(&n); err != nil {
		t.Fatalf("count periods: %v", err)
	}
	return n
}

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

// split is the current plan's split of n events, which every subscription here is on.
func split(n int64) []int64 {
	plan := entitlement.CurrentPlan()
	return meter.Split(n, plan.FreeEvents, plan.TierUpTo)
}

func zeros() []int64 { return make([]int64, entitlement.CurrentPlan().Tiers()) }

func plus(a, b []int64) []int64 {
	out := slices.Clone(a)
	for k := range b {
		out[k] += b[k]
	}
	return out
}

// The current plan's placeholder tiers split 2.1M events above a 100k allowance into
// 1.9M in tier 1 and 100k in tier 2 (see entitlement's catalog constants).
func TestStatesTheWindowsTiers(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3).Add(2*time.Hour), d(11, 3).Add(2*time.Hour)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 2), 5_000_000) // before the window: never billed here
	f.usage(t, d(10, 3), 1_100_000)
	f.usage(t, d(10, 10), 1_000_000)

	now := d(10, 10).Add(13 * time.Hour)
	report, err := f.run(t, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Stated != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; want one statement", report, len(f.meter.sent))
	}
	got := f.meter.sent[0]
	if !slices.Equal(got.TierEvents, split(2_100_000)) || got.CustomerID != "cus_1" {
		t.Fatalf("stated %+v, want tiers %v for cus_1", got, split(2_100_000))
	}
	// Stamped with the tick's start: before the period's end by the freeze rule, and
	// inside the provider's hour however long the pass runs.
	if !got.At.Equal(now) {
		t.Errorf("stamped %s, want the tick's start %s", got.At, now)
	}
	if _, _, acked := f.ledger(t, start); !acked {
		t.Error("a statement that landed must be acked")
	}
}

func TestStatesNothingNewWhenNothingGrew(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	now := d(10, 6)
	if _, err := f.run(t, now); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	report, err := f.run(t, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if report.Unchanged != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; an unchanged period must make no call", report, len(f.meter.sent))
	}
}

// A tick that states nothing still saw the subscription live, which is what a change
// of subscription later reads to know which days were this one's.
func TestAQuietTickStillRecordsTheDaysItSaw(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	report, err := f.run(t, d(10, 9).Add(13*time.Hour))
	if err != nil || report.Unchanged != 1 {
		t.Fatalf("report = %+v, err = %v; want an unchanged tick", report, err)
	}
	if _, _, through := f.window(t, start); !through.Equal(d(10, 10)) {
		t.Fatalf("summed through %s, want the end of the day the quiet tick ran", through)
	}
}

// An erasure or a late recount can lower a window's sum; what the provider holds
// cannot go down, so neither may the ledger.
func TestAStatementNeverGoesDown(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	before, _, _ := f.ledger(t, start)
	f.usage(t, d(10, 5), 1_000_000)
	report, err := f.run(t, d(10, 6).Add(time.Hour))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	after, _, _ := f.ledger(t, start)
	if !slices.Equal(before, after) || report.Unchanged != 1 {
		t.Fatalf("ledger went from %v to %v (report %+v); a lower recount must change nothing", before, after, report)
	}
}

// From the nominal end on, nothing is stated: the provider is renewing, late.
func TestFreezesFromTheNominalRenewal(t *testing.T) {
	for name, after := range map[string]time.Duration{"at the end": 0, "past it": 30 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			start, end := d(10, 3).Add(2*time.Hour), d(11, 3).Add(2*time.Hour)
			f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
			f.usage(t, d(10, 5), 3_000_000)
			report, err := f.run(t, end.Add(after))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if report.Frozen != 1 || len(f.meter.attempts) != 0 {
				t.Fatalf("report = %+v, offered %d; nothing may be stated from current_period_end on",
					report, len(f.meter.attempts))
			}
		})
	}
}

// An un-acked statement is re-sent only while its period is open. Past the nominal
// renewal the provider is processing the new period, and a statement stamped now
// could land in it — billing the old period's whole count a second time. What this
// costs is the accepted under-bill: the next period counts the statement as sent.
func TestAnUnackedStatementIsNotResentPastTheRenewal(t *testing.T) {
	f := setup(t)
	start, end, next := d(10, 3), d(11, 3), d(12, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	f.meter.fail = errors.New("provider down")
	if _, err := f.run(t, end.Add(-30*time.Minute)); err == nil {
		t.Fatal("a failed ingest must fail the pass")
	}
	f.meter.fail = nil
	report, err := f.run(t, end.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Run past the renewal: %v", err)
	}
	if report.Frozen != 1 || report.Resent != 0 || len(f.meter.sent) != 0 {
		t.Fatalf("report = %+v, sent %d; nothing may be re-sent past current_period_end", report, len(f.meter.sent))
	}

	f.renew(t, end, next)
	if _, err := f.run(t, end.Add(2*time.Hour)); err != nil {
		t.Fatalf("Run in the next period: %v", err)
	}
	if _, carry, _ := f.ledger(t, end); !slices.Equal(carry, zeros()) {
		t.Fatalf("carry = %v; the un-acked statement counts as sent, so nothing carries", carry)
	}
}

// The shortfall is carried and reaches the provider: what is stated is the period's
// own split plus the carry, not either alone.
func TestCarriesTheShortfallAfterARoll(t *testing.T) {
	f := setup(t)
	oct, nov, dec := d(10, 3), d(11, 3), d(12, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", oct, nov)
	f.usage(t, d(10, 20), 3_000_000)
	if _, err := f.run(t, d(10, 21)); err != nil {
		t.Fatalf("October Run: %v", err)
	}
	// Late metering for October lands after its last statement.
	f.usage(t, d(11, 2), 500_000)
	f.renew(t, nov, dec)
	f.usage(t, d(11, 10), 200_000)

	if _, err := f.run(t, d(11, 10).Add(time.Hour)); err != nil {
		t.Fatalf("November Run: %v", err)
	}
	shortfall := []int64{0, 500_000, 0, 0, 0, 0}
	if want := sumDiff(split(3_500_000), split(3_000_000)); !slices.Equal(shortfall, want) {
		t.Fatalf("test arithmetic: shortfall %v, want %v", shortfall, want)
	}
	own, carry, _ := f.ledger(t, nov)
	if !slices.Equal(carry, shortfall) || !slices.Equal(own, split(200_000)) {
		t.Fatalf("own %v carry %v, want %v and October's shortfall %v", own, carry, split(200_000), shortfall)
	}
	if got := f.meter.last(t, "cus_1").TierEvents; !slices.Equal(got, plus(split(200_000), shortfall)) {
		t.Fatalf("stated %v, want own plus carry %v", got, plus(split(200_000), shortfall))
	}
}

func sumDiff(a, b []int64) []int64 {
	out := slices.Clone(a)
	for k := range b {
		out[k] -= b[k]
	}
	return out
}

// Each window's shortfall is carried once, by the window after it, measured against
// what that window stated as its own: never re-carried a period later, and never
// netted against the carry it took in itself. The carry is recomputed every tick,
// so a day metered after the period's first statement still arrives.
func TestEachShortfallIsCarriedOnce(t *testing.T) {
	f := setup(t)
	oct, nov, dec, jan := d(10, 3), d(11, 3), d(12, 3), time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", oct, nov)
	f.usage(t, d(10, 20), 3_000_000)
	if _, err := f.run(t, d(10, 21)); err != nil {
		t.Fatalf("October Run: %v", err)
	}
	f.renew(t, nov, dec)
	f.usage(t, d(11, 10), 2_200_000)
	if _, err := f.run(t, d(11, 10).Add(13*time.Hour)); err != nil {
		t.Fatalf("November Run: %v", err)
	}
	// October's late day arrives after November's first statement.
	f.usage(t, d(11, 2), 500_000)
	if _, err := f.run(t, d(11, 11).Add(13*time.Hour)); err != nil {
		t.Fatalf("second November Run: %v", err)
	}
	octShortfall := sumDiff(split(3_500_000), split(3_000_000))
	if _, carry, _ := f.ledger(t, nov); !slices.Equal(carry, octShortfall) {
		t.Fatalf("November carries %v, want October's late shortfall %v", carry, octShortfall)
	}

	// November ends 100k short of what it stated as its own.
	f.usage(t, d(12, 1), 100_000)
	f.renew(t, dec, jan)
	if _, err := f.run(t, d(12, 3).Add(13*time.Hour)); err != nil {
		t.Fatalf("December Run: %v", err)
	}
	novShortfall := sumDiff(split(2_300_000), split(2_200_000))
	if _, carry, _ := f.ledger(t, dec); !slices.Equal(carry, novShortfall) {
		t.Fatalf("December carries %v, want November's own shortfall %v alone", carry, novShortfall)
	}
}

// The deal is bought while the old subscription still runs, and is live once that
// one ends. The days the old one held stay its own.
func TestACutoversOverlapIsStatedOnce(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_old", d(9, 15), d(10, 15))
	f.usage(t, d(10, 5), 400_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("old Run: %v", err)
	}
	if _, err := f.run(t, d(10, 14).Add(13*time.Hour)); err != nil {
		t.Fatalf("old Run on its last day: %v", err)
	}
	f.subscribe(t, entitlement.SlugUsage, "cus_new", d(10, 3), d(11, 3))
	f.usage(t, d(10, 16), 300_000)
	if _, err := f.run(t, d(10, 16).Add(13*time.Hour)); err != nil {
		t.Fatalf("new Run: %v", err)
	}
	if from, to, _ := f.window(t, d(10, 3)); !from.Equal(d(10, 15)) || !to.Equal(d(11, 3)) {
		t.Fatalf("new window [%s, %s), want [15 Oct, 3 Nov): 5 October is the old window's", from, to)
	}
	if got := f.meter.last(t, "cus_new").TierEvents; !slices.Equal(got, split(300_000)) {
		t.Fatalf("stated %v to the new customer, want its own days alone, %v", got, split(300_000))
	}
}

// The deal is bought on 13 October and refused while the usage subscription is
// live; that one renews on 15 October and is metered once in its new period before
// it is cancelled, and the deal goes live. The deal's first window follows the
// days the old subscription was seen live, not the old window's nominal end — so
// its second period follows its first, rather than re-billing the old window.
func TestACutoverStraddlingTheOldRenewalIsStatedOnce(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_old", d(9, 15), d(10, 15))
	f.usage(t, d(10, 5), 400_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("old Run: %v", err)
	}
	if _, err := f.run(t, d(10, 14).Add(13*time.Hour)); err != nil {
		t.Fatalf("old Run on its last day: %v", err)
	}
	f.renew(t, d(10, 15), d(11, 15))
	f.usage(t, d(10, 15), 500_000)
	if _, err := f.run(t, d(10, 15).Add(13*time.Hour)); err != nil {
		t.Fatalf("old Run after its renewal: %v", err)
	}

	f.subscribe(t, entitlement.SlugUsage, "cus_new", d(10, 13), d(11, 13))
	f.usage(t, d(10, 16), 300_000)
	if _, err := f.run(t, d(10, 16).Add(13*time.Hour)); err != nil {
		t.Fatalf("deal's first Run: %v", err)
	}
	if from, to, _ := f.window(t, d(10, 13)); !from.Equal(d(10, 16)) || !to.Equal(d(11, 13)) {
		t.Fatalf("deal's first window [%s, %s), want [16 Oct, 13 Nov): 15 October is the renewed window's", from, to)
	}
	if got := f.meter.last(t, "cus_new").TierEvents; !slices.Equal(got, split(300_000)) {
		t.Fatalf("stated %v to the deal, want its own day alone, %v", got, split(300_000))
	}
	// The renewed window's late growth arrives through the deal's carry, once.
	f.usage(t, d(10, 15), 600_000)
	if _, err := f.run(t, d(10, 17).Add(13*time.Hour)); err != nil {
		t.Fatalf("deal's second Run: %v", err)
	}
	late := sumDiff(split(600_000), split(500_000))
	if _, carry, _ := f.ledger(t, d(10, 13)); !slices.Equal(carry, late) {
		t.Fatalf("deal carries %v, want the renewed window's late growth %v", carry, late)
	}

	// The deal's first window ends short of what it stated as its own, and its
	// renewal follows that window — not the old subscription's, whose days were
	// carried once already.
	f.usage(t, d(11, 1), 400_000)
	f.renew(t, d(11, 13), d(12, 13))
	f.usage(t, d(11, 13), 1_000_000)
	if _, err := f.run(t, d(11, 13).Add(13*time.Hour)); err != nil {
		t.Fatalf("deal's renewal Run: %v", err)
	}
	if from, to, _ := f.window(t, d(11, 13)); !from.Equal(d(11, 13)) || !to.Equal(d(12, 13)) {
		t.Fatalf("deal's second window [%s, %s), want [13 Nov, 13 Dec): it follows the deal's own first window", from, to)
	}
	shortfall := sumDiff(split(700_000), split(300_000))
	if _, carry, _ := f.ledger(t, d(11, 13)); !slices.Equal(carry, shortfall) {
		t.Fatalf("deal's renewal carries %v, want its first window's shortfall %v", carry, shortfall)
	}
	if got := f.meter.last(t, "cus_new").TierEvents; !slices.Equal(got, plus(split(1_000_000), shortfall)) {
		t.Fatalf("stated %v, want the period's own days and the deal's carry, %v: nothing of the old windows again",
			got, plus(split(1_000_000), shortfall))
	}
}

// Cancelled at once and bought again before the old period's nominal end: the days
// between were the org's on no subscription, and the new subscription's are its
// own — neither reaches the provider through a carry of the old window.
func TestAGapIsNotBilledThroughTheCarry(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_a", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 1_000_000)
	if _, err := f.run(t, d(10, 5).Add(13*time.Hour)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := f.run(t, d(10, 9).Add(13*time.Hour)); err != nil {
		t.Fatalf("Run before the cancellation: %v", err)
	}
	f.cancel(t)
	f.usage(t, d(10, 12), 5_000_000) // on no subscription
	f.subscribe(t, entitlement.SlugUsage, "cus_c", d(10, 20), d(11, 20))
	f.usage(t, d(10, 22), 300_000)
	if _, err := f.run(t, d(10, 22).Add(13*time.Hour)); err != nil {
		t.Fatalf("Run after re-subscribing: %v", err)
	}
	if from, _, _ := f.window(t, d(10, 20)); !from.Equal(d(10, 20)) {
		t.Fatalf("new window starts %s, want its own start, 20 October", from)
	}
	if got := f.meter.last(t, "cus_c").TierEvents; !slices.Equal(got, split(300_000)) {
		t.Fatalf("stated %v, want the new subscription's own days alone, %v", got, split(300_000))
	}
}

func TestAFailedIngestIsResentUnchanged(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	f.meter.fail = errors.New("provider down")
	if _, err := f.run(t, d(10, 6)); err == nil {
		t.Fatal("a failed ingest must fail the pass")
	}
	if _, _, acked := f.ledger(t, start); acked {
		t.Fatal("the write-ahead row must stay un-acked when the ingest failed")
	}
	f.meter.fail = nil
	recovery := d(10, 6).Add(time.Hour)
	report, err := f.run(t, recovery)
	if err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if report.Resent != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; the un-acked statement must be re-sent", report, len(f.meter.sent))
	}
	// The same counts and event id, stamped with the tick that sent it: the original
	// stamp could be past the provider's hour by now.
	resent := f.meter.sent[0]
	if resent.EventID != f.meter.attempts[0].EventID || !resent.At.Equal(recovery) {
		t.Fatalf("re-sent %s at %s, want %s at %s", resent.EventID, resent.At, f.meter.attempts[0].EventID, recovery)
	}
	if _, _, acked := f.ledger(t, start); !acked {
		t.Error("the re-sent statement must end acked")
	}
}

func TestAFailedAckIsResent(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	// Moving stated_at under the ingest makes the ack match nothing.
	f.meter.during = func() {
		if _, err := f.pg.PgW.Exec(t.Context(),
			`update billing_meter_periods set stated_at = stated_at - interval '1 second' where org_id = $1`, f.org); err != nil {
			t.Errorf("disturb ledger: %v", err)
		}
	}
	if _, err := f.run(t, d(10, 6)); err == nil {
		t.Fatal("a failed ack must fail the pass")
	}
	f.meter.during = nil
	if _, err := f.run(t, d(10, 6).Add(time.Hour)); err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if len(f.meter.sent) != 2 || f.meter.sent[0].EventID != f.meter.sent[1].EventID {
		t.Fatalf("sent %+v; the re-send must repeat the first statement's event id", f.meter.sent)
	}
}

// Nothing is sent before its row is written: a write-ahead that fails leaves the
// provider holding nothing the ledger does not show.
func TestAFailedWriteAheadSendsNothing(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`alter table billing_meter_periods add constraint refuse_writes check (false) not valid`); err != nil {
		t.Fatalf("refuse writes: %v", err)
	}
	report, err := f.run(t, d(10, 6))
	if err == nil || report.Failed != 1 || len(f.meter.attempts) != 0 {
		t.Fatalf("report = %+v, err = %v, offered %d; want a failure and nothing offered",
			report, err, len(f.meter.attempts))
	}
	if _, err := f.pg.PgW.Exec(t.Context(), `alter table billing_meter_periods drop constraint refuse_writes`); err != nil {
		t.Fatalf("allow writes: %v", err)
	}
	if report, err := f.run(t, d(10, 6).Add(time.Hour)); err != nil || report.Stated != 1 {
		t.Fatalf("report = %+v, err = %v; the next tick states it", report, err)
	}
}

// The failing org is metered first, so a loop that stopped at its first failure
// would never reach the other.
func TestOneOrgFailingDoesNotStopTheOthers(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 3_000_000)
	// On a slug the catalog does not know, so it cannot be split.
	other, _ := f.newOrg(t, "00000000000000000000")
	f.subscribeOrg(t, other, "retired-long-ago", "cus_2", d(10, 3), d(11, 3))
	report, err := f.run(t, d(10, 6))
	if err == nil || report.Failed != 1 || report.Stated != 1 {
		t.Fatalf("report = %+v, err = %v; want one failure beside one statement", report, err)
	}
}

// A deal splits over the plan pinned on its row, so its ledger records that plan:
// carryFrom and the dashboard's tier bounds both look the layout up from it, and
// "custom" names no layout.
func TestCarriesADealsShortfallOverItsPinnedPlan(t *testing.T) {
	f := setup(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id, base_plan_slug)
		 values ($1, 'custom', 'prod_deal', $2)`, f.org, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed deal: %v", err)
	}
	plan, _ := entitlement.PlanBySlug(entitlement.SlugUsage)
	oct, nov, dec := d(10, 3), d(11, 3), d(12, 3)
	f.subscribe(t, entitlement.SlugCustom, "cus_1", oct, nov)
	f.usage(t, d(10, 20), 3_000_000)
	if _, err := f.run(t, d(10, 21)); err != nil {
		t.Fatalf("October Run: %v", err)
	}
	f.usage(t, d(11, 2), 500_000)
	f.renew(t, nov, dec)
	f.usage(t, d(11, 10), 200_000)
	if _, err := f.run(t, d(11, 10).Add(time.Hour)); err != nil {
		t.Fatalf("November Run: %v", err)
	}

	var splitBy string
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select plan_slug from billing_meter_periods where org_id = $1 and period_start = $2`, f.org, oct).Scan(&splitBy); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if splitBy != plan.Slug {
		t.Errorf("October's period records %q, want the plan it split by, %q", splitBy, plan.Slug)
	}
	if _, carry, _ := f.ledger(t, nov); !slices.Equal(carry, sumDiff(split(3_500_000), split(3_000_000))) {
		t.Fatalf("carry = %v, want October's shortfall", carry)
	}
}

// A deal allowing fewer events than the default still bills from its own allowance.
func TestADealsAllowanceMovesTierOne(t *testing.T) {
	f := setup(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id, included_events_override, base_plan_slug)
		 values ($1, 'custom', 'prod_deal', 1, $2)`, f.org, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed deal: %v", err)
	}
	f.subscribe(t, entitlement.SlugCustom, "cus_1", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 50_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.meter.sent[0].TierEvents[0]; got != 49_999 {
		t.Fatalf("tier 1 = %d, want 49,999 (everything past the deal's one free event)", got)
	}
}

// A deal re-set between periods changes the next period's terms, not the last one's:
// the old window's shortfall is re-split under the allowance it was stated under.
func TestCarriesUnderTheAllowanceTheWindowWasStatedUnder(t *testing.T) {
	f := setup(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id, included_events_override, base_plan_slug)
		 values ($1, 'custom', 'prod_deal', 1, $2)`, f.org, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed deal: %v", err)
	}
	oct, nov, dec := d(10, 3), d(11, 3), d(12, 3)
	f.subscribe(t, entitlement.SlugCustom, "cus_1", oct, nov)
	f.usage(t, d(10, 5), 50_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("October Run: %v", err)
	}
	f.usage(t, d(11, 2), 10_000) // October's, metered late
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_entitlements set included_events_override = 1000000 where org_id = $1`, f.org); err != nil {
		t.Fatalf("re-set the deal: %v", err)
	}
	f.renew(t, nov, dec)
	if _, err := f.run(t, d(11, 3).Add(13*time.Hour)); err != nil {
		t.Fatalf("November Run: %v", err)
	}
	if _, carry, _ := f.ledger(t, nov); carry[0] != 10_000 {
		t.Fatalf("carry = %v, want October's 10,000 late events past its own one-event allowance", carry)
	}
}

// A period whose end the provider pushes out, start unchanged, is summed past the
// end its row last recorded. A quiet tick then must not record more than that row
// covers, or its CHECK fails the org on every tick until something grows.
func TestAPeriodWhoseEndMovesOutStaysMeterable(t *testing.T) {
	f := setup(t)
	start := d(10, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, d(11, 3))
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set current_period_end = $1 where org_id = $2`, d(11, 10), f.org); err != nil {
		t.Fatalf("extend the period: %v", err)
	}
	report, err := f.run(t, d(11, 4).Add(13*time.Hour))
	if err != nil || report.Unchanged != 1 {
		t.Fatalf("report = %+v, err = %v; want a quiet tick that succeeds", report, err)
	}
}

// seedPeriod writes a ledger row as an earlier pass would have, split by plan.
func (f *fixture) seedPeriod(t *testing.T, start, from, to time.Time, plan, subID string, own []int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_meter_periods (acked, allowance, carry_events, org_id, own_events, period_start,
		   plan_slug, provider_customer_id, provider_sub_id, stated_at, summed_through, window_end, window_start)
		 values (true, 100000, $1, $2, $3, $4, $5, 'cus_1', $6, $4, $7, $7, $8)`,
		make([]int64, len(own)), f.org, own, start, plan, subID, to, from); err != nil {
		t.Fatalf("seed period: %v", err)
	}
}

// Another plan's tiers cover other bounds: the max of two splits could state more
// events than were sent, and a max meter never comes back down. Refused, loudly.
func TestARestatementUnderAnotherPlanIsRefused(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	subID := f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.seedPeriod(t, start, start, end, "usage-2019-01", subID, []int64{1, 0, 0, 0, 0, 0})
	f.usage(t, d(10, 5), 13_000_000)
	report, err := f.run(t, d(10, 6))
	if err == nil || report.Failed != 1 || len(f.meter.attempts) != 0 {
		t.Fatalf("report = %+v, err = %v, offered %d; want the restatement refused",
			report, err, len(f.meter.attempts))
	}
}

// A shortfall lands in the same tier of the next period, which under another plan
// covers other bounds. Nothing carries: the under-billing direction.
func TestNothingCarriesAcrossPlans(t *testing.T) {
	f := setup(t)
	subID := f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	f.seedPeriod(t, d(9, 3), d(9, 3), d(10, 3), "usage-2019-01", subID, zeros())
	f.usage(t, d(9, 20), 5_000_000) // the old window's, under the old plan
	f.usage(t, d(10, 5), 300_000)
	if _, err := f.run(t, d(10, 6)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.meter.last(t, "cus_1").TierEvents; !slices.Equal(got, split(300_000)) {
		t.Fatalf("stated %v, want this period's own days alone, %v", got, split(300_000))
	}
}

// The meter sums the day cells pug cron usage writes. Unrun or stalled, those cells
// would bill every subscriber the fee alone: nothing is stated, and the pass fails.
func TestStatesNothingFromStaleUsage(t *testing.T) {
	f := setup(t)
	now := d(10, 6)
	// No subscriber: nothing to gate, so a deployment that never meters usage is fine.
	if _, err := f.svc.Run(t.Context(), now); err != nil {
		t.Fatalf("Run with nothing to meter: %v", err)
	}
	f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.svc.Run(t.Context(), now); !errors.Is(err, meter.ErrUsageStale) {
		t.Fatalf("never metered: err = %v, want ErrUsageStale", err)
	}
	f.usageMetered(t, now.Add(-4*time.Hour))
	if _, err := f.svc.Run(t.Context(), now); !errors.Is(err, meter.ErrUsageStale) {
		t.Fatalf("metered four hours ago: err = %v, want ErrUsageStale", err)
	}
	if len(f.meter.attempts) != 0 || f.periods(t, f.org) != 0 {
		t.Fatalf("offered %d statements over stale counts; want none", len(f.meter.attempts))
	}
	f.usageMetered(t, now.Add(-time.Hour))
	if report, err := f.svc.Run(t.Context(), now); err != nil || report.Stated != 1 {
		t.Fatalf("report = %+v, err = %v; fresh counts are stated", report, err)
	}
}

// The subscription is listed when the pass starts and the entitlement read when
// the org's turn comes. In between it can end, or give way to another: the org is
// left to the next tick rather than stated on terms that are not its period's, and
// that is not a failure.
func TestASubscriptionThatChangesMidPassIsLeftToTheNextTick(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, f *fixture, org string){
		"cancelled": func(t *testing.T, f *fixture, org string) { f.cancelOrg(t, org) },
		"replaced": func(t *testing.T, f *fixture, org string) {
			f.subscribeOrg(t, org, entitlement.SlugUsage, "cus_new", d(10, 4), d(11, 4))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
			f.usage(t, d(10, 5), 3_000_000)
			// Metered second: its id sorts after the fixture's org.
			other, proj := f.newOrg(t, "zzzzzzzzzzzzzzzzzzzz")
			f.subscribeOrg(t, other, entitlement.SlugUsage, "cus_2", d(10, 3), d(11, 3))
			f.usageOf(t, proj, d(10, 5), 3_000_000)
			changed := false
			f.meter.during = func() {
				if !changed {
					changed = true
					change(t, f, other)
				}
			}
			report, err := f.run(t, d(10, 6))
			if err != nil || report.Stated != 1 || report.Superseded != 1 || report.Failed != 0 {
				t.Fatalf("report = %+v, err = %v; want one statement and one org left to the next tick", report, err)
			}
			if f.periods(t, other) != 0 {
				t.Fatal("the changed org's period was written")
			}
		})
	}
}

// Past its deadline the pass stops and says how far it got, rather than reporting
// every org it had not reached as a failed read.
func TestAPassPastItsDeadlineStops(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	other, _ := f.newOrg(t, "zzzzzzzzzzzzzzzzzzzz")
	f.subscribeOrg(t, other, entitlement.SlugUsage, "cus_2", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 3_000_000)
	f.usageMetered(t, d(10, 6).Add(-10*time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.meter.during = cancel
	report, err := f.svc.Run(ctx, d(10, 6))
	if !errors.Is(err, context.Canceled) || report.Orgs != 1 {
		t.Fatalf("report = %+v, err = %v; want the pass cut short after the first org", report, err)
	}
	if f.periods(t, other) != 0 {
		t.Fatal("an org past the deadline was metered")
	}
}

// A live subscription the provider has reported no period for cannot be stated.
// Left out of the work list it would never be metered while the pass exited 0.
func TestALiveSubscriptionWithNoPeriodFails(t *testing.T) {
	f := setup(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', $1, $2, $3, 100, 'dodo', 'cus_1', 'active', 'sub_x', now(), 'active')`,
		xid.New().String(), f.org, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	report, err := f.run(t, d(10, 6))
	if err == nil || report.Orgs != 1 || report.Failed != 1 {
		t.Fatalf("report = %+v, err = %v; want the org listed and failed", report, err)
	}
}

// Only the configured provider's live subscriptions are this pass's to state.
func TestMetersOnlyTheProvidersLiveSubscriptions(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	f.cancel(t)
	other, _ := f.newOrg(t, "zzzzzzzzzzzzzzzzzzzz")
	f.subscribeOrg(t, other, entitlement.SlugUsage, "cus_2", d(10, 3), d(11, 3))
	if _, err := f.pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set provider = 'paddle' where org_id = $1`, other); err != nil {
		t.Fatalf("move to another provider: %v", err)
	}
	if report, err := f.run(t, d(10, 6)); err != nil || report.Orgs != 0 {
		t.Fatalf("report = %+v, err = %v; want nothing listed", report, err)
	}
}
