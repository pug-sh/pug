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
	sent []corebilling.UsageStatement
	fail error
	// during runs inside IngestUsage, to disturb the ledger mid-statement.
	during func()
}

func (r *recorder) IngestUsage(_ context.Context, s corebilling.UsageStatement) error {
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
	w := dbwrite.New(pg.PgW)
	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj := xid.New().String()
	if _, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{ID: proj, OrgID: org.ID, DisplayName: "p"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	ents, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("entitlement service: %v", err)
	}
	rec := &recorder{}
	return &fixture{pg: pg, org: org.ID, proj: proj, meter: rec,
		svc: meter.NewService(pg.PgRO, pg.PgW, ents, rec, "dodo")}
}

func (f *fixture) usage(t *testing.T, day time.Time, n int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into usage_daily (day, event_count, org_id, project_id) values ($1, $2, $3, $4)
		 on conflict (project_id, day) do update set event_count = excluded.event_count`,
		day, n, f.org, f.proj); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

func (f *fixture) subscribe(t *testing.T, plan, customer string, start, end time.Time) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(), `delete from billing_subscriptions where org_id = $1`, f.org); err != nil {
		t.Fatalf("clear subscriptions: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   plan_slug, price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ('USD', $1, $2, $3, $4, $5, 100, 'dodo', $6, 'active', $7, now(), 'active')`,
		end, start, xid.New().String(), f.org, plan, customer, "sub_"+xid.New().String()); err != nil {
		t.Fatalf("seed subscription: %v", err)
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

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

// The current plan's placeholder tiers split 2.1M events above a 100k allowance into
// 1.9M in tier 1 and 100k in tier 2 (see entitlement's catalog constants).
func TestStatesTheWindowsTiers(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3).Add(2*time.Hour), d(11, 3).Add(2*time.Hour)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 2), 5_000_000) // before the window: never billed here
	f.usage(t, d(10, 3), 1_100_000)
	f.usage(t, d(10, 10), 1_000_000)

	report, err := f.svc.Run(t.Context(), d(10, 10).Add(13*time.Hour))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Stated != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; want one statement", report, len(f.meter.sent))
	}
	got := f.meter.sent[0]
	plan := entitlement.CurrentPlan()
	want := meter.Split(2_100_000, plan.FreeEvents, plan.TierUpTo)
	if !slices.Equal(got.TierEvents, want) || got.CustomerID != "cus_1" {
		t.Fatalf("stated %+v, want tiers %v for cus_1", got, want)
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
	if _, err := f.svc.Run(t.Context(), now); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	report, err := f.svc.Run(t.Context(), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if report.Unchanged != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; an unchanged period must make no call", report, len(f.meter.sent))
	}
}

// An erasure or a late recount can lower a window's sum; what the provider holds
// cannot go down, so neither may the ledger.
func TestAStatementNeverGoesDown(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	if _, err := f.svc.Run(t.Context(), d(10, 6)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	before, _, _ := f.ledger(t, start)
	f.usage(t, d(10, 5), 1_000_000)
	report, err := f.svc.Run(t.Context(), d(10, 6).Add(time.Hour))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	after, _, _ := f.ledger(t, start)
	if !slices.Equal(before, after) || report.Unchanged != 1 {
		t.Fatalf("ledger went from %v to %v (report %+v); a lower recount must change nothing", before, after, report)
	}
}

func TestFreezesPastTheNominalRenewal(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	report, err := f.svc.Run(t.Context(), end.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Frozen != 1 || len(f.meter.sent) != 0 {
		t.Fatalf("report = %+v, sent %d; nothing may be stated past current_period_end", report, len(f.meter.sent))
	}
}

// An un-acked statement is re-sent only while its period is open. Past the nominal
// renewal the provider is processing the new period, and a statement stamped now
// could land in it — billing the old period's whole count a second time. What this
// costs is the accepted under-bill: up to one tick's growth.
func TestAnUnackedStatementIsNotResentPastTheRenewal(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	f.meter.fail = errors.New("provider down")
	if _, err := f.svc.Run(t.Context(), end.Add(-30*time.Minute)); err == nil {
		t.Fatal("a failed ingest must fail the pass")
	}
	f.meter.fail = nil
	report, err := f.svc.Run(t.Context(), end.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Run past the renewal: %v", err)
	}
	if report.Frozen != 1 || report.Resent != 0 || len(f.meter.sent) != 0 {
		t.Fatalf("report = %+v, sent %d; nothing may be re-sent past current_period_end", report, len(f.meter.sent))
	}
}

func TestCarriesTheShortfallAfterARoll(t *testing.T) {
	f := setup(t)
	plan := entitlement.CurrentPlan()
	oct, nov, dec := d(10, 3), d(11, 3), d(12, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", oct, nov)
	f.usage(t, d(10, 20), 3_000_000)
	if _, err := f.svc.Run(t.Context(), d(10, 21)); err != nil {
		t.Fatalf("October Run: %v", err)
	}
	// Late metering for October lands after its last statement.
	f.usage(t, d(11, 2), 500_000)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", nov, dec)
	f.usage(t, d(11, 10), 200_000)

	if _, err := f.svc.Run(t.Context(), d(11, 10).Add(time.Hour)); err != nil {
		t.Fatalf("November Run: %v", err)
	}
	final := meter.Split(3_500_000, plan.FreeEvents, plan.TierUpTo)
	stated := meter.Split(3_000_000, plan.FreeEvents, plan.TierUpTo)
	_, carry, _ := f.ledger(t, nov)
	for k := range carry {
		if carry[k] != final[k]-stated[k] {
			t.Fatalf("carry = %v, want October's shortfall %v - %v", carry, final, stated)
		}
	}
}

func TestACutoversOverlapIsStatedOnce(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_old", d(9, 15), d(10, 15))
	f.usage(t, d(10, 5), 400_000)
	if _, err := f.svc.Run(t.Context(), d(10, 6)); err != nil {
		t.Fatalf("old Run: %v", err)
	}
	// The deal was bought on 3 October and becomes live once the old one ends.
	f.subscribe(t, entitlement.SlugUsage, "cus_new", d(10, 3), d(11, 3))
	if _, err := f.svc.Run(t.Context(), d(10, 16)); err != nil {
		t.Fatalf("new Run: %v", err)
	}
	var start time.Time
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select window_start from billing_meter_periods where org_id = $1 and period_start = $2`,
		f.org, d(10, 3)).Scan(&start); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !start.Equal(d(10, 15)) {
		t.Fatalf("new window starts %s, want 15 October: 5 October is the old window's", start)
	}
}

func TestAFailedIngestIsResentUnchanged(t *testing.T) {
	f := setup(t)
	start, end := d(10, 3), d(11, 3)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", start, end)
	f.usage(t, d(10, 5), 3_000_000)
	f.meter.fail = errors.New("provider down")
	if _, err := f.svc.Run(t.Context(), d(10, 6)); err == nil {
		t.Fatal("a failed ingest must fail the pass")
	}
	if _, _, acked := f.ledger(t, start); acked {
		t.Fatal("the write-ahead row must stay un-acked when the ingest failed")
	}
	f.meter.fail = nil
	report, err := f.svc.Run(t.Context(), d(10, 6).Add(time.Hour))
	if err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if report.Resent != 1 || len(f.meter.sent) != 1 {
		t.Fatalf("report = %+v, sent %d; the un-acked statement must be re-sent", report, len(f.meter.sent))
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
	if _, err := f.svc.Run(t.Context(), d(10, 6)); err == nil {
		t.Fatal("a failed ack must fail the pass")
	}
	f.meter.during = nil
	if _, err := f.svc.Run(t.Context(), d(10, 6).Add(time.Hour)); err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if len(f.meter.sent) != 2 || f.meter.sent[0].EventID != f.meter.sent[1].EventID {
		t.Fatalf("sent %+v; the re-send must repeat the first statement's event id", f.meter.sent)
	}
}

func TestOneOrgFailingDoesNotStopTheOthers(t *testing.T) {
	f := setup(t)
	f.subscribe(t, entitlement.SlugUsage, "cus_1", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 3_000_000)
	// A second org on a slug the catalog does not know cannot be split.
	w := dbwrite.New(f.pg.PgW)
	other, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "b"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   plan_slug, price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ('USD', $1, $2, $3, $4, 'retired-long-ago', 100, 'dodo', 'cus_2', 'active', 'sub_2', now(), 'active')`,
		d(11, 3), d(10, 3), xid.New().String(), other.ID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	report, err := f.svc.Run(t.Context(), d(10, 6))
	if err == nil || report.Failed != 1 || report.Stated != 1 {
		t.Fatalf("report = %+v, err = %v; want one failure beside one statement", report, err)
	}
}

// A deal allowing fewer events than the default still bills from its own allowance.
func TestADealsAllowanceMovesTierOne(t *testing.T) {
	f := setup(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id, included_events_override)
		 values ($1, 'custom', 'prod_deal', 1)`, f.org); err != nil {
		t.Fatalf("seed deal: %v", err)
	}
	f.subscribe(t, entitlement.SlugCustom, "cus_1", d(10, 3), d(11, 3))
	f.usage(t, d(10, 5), 50_000)
	if _, err := f.svc.Run(t.Context(), d(10, 6)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.meter.sent[0].TierEvents[0]; got != 49_999 {
		t.Fatalf("tier 1 = %d, want 49,999 (everything past the deal's one free event)", got)
	}
}
