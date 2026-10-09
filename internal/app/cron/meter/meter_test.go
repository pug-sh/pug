package meter

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coremeter "github.com/pug-sh/pug/internal/core/billing/meter"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// seedLive gives a fresh org one live subscription: whether a pass that cannot
// meter is healthy turns on whether anybody is paying.
func seedLive(t *testing.T, pg *testutil.TestPostgres) {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   plan_slug, price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ('USD', now() + interval '20 days', now() - interval '10 days', $1, $2, $3, 100, 'dodo',
		         'cus_1', 'active', $4, now(), 'active')`,
		xid.New().String(), org.ID, entitlement.SlugUsage, "sub_"+xid.New().String()); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

func runEnv(t *testing.T, pg *testutil.TestPostgres, enabled, provider string) {
	t.Helper()
	t.Setenv("PUG_BILLING_ENABLED", enabled)
	t.Setenv("PUG_BILLING_PROVIDER", provider)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
}

// Off means nothing is metered and no provider is built, live subscriptions or not:
// an unknown provider name would fail construction, so a clean exit proves the pass
// stopped first.
func TestRunIsANoOpWithBillingOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	seedLive(t, pg)
	runEnv(t, pg, "false", "stripe")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with billing off: %v", err)
	}
}

// Billing on with no provider is a supported mode while nobody pays. Once somebody
// does, a pass that cannot state their usage bills them the fee alone, and the
// CronJob must say so.
func TestRunWithoutAProviderFailsOnlyWhenSomebodyPays(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	runEnv(t, pg, "true", "")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run with no provider and no subscriber: %v", err)
	}
	seedLive(t, pg)
	if err := Run(t.Context()); err == nil {
		t.Fatal("Run with no provider and a live subscription must fail")
	}
}

// checkRecorder is a UsageMeter that records each check's tier count and fails on
// demand.
type checkRecorder struct {
	tiers []int
	fail  error
}

func (*checkRecorder) IngestUsage(context.Context, corebilling.UsageStatement) error { return nil }

func (c *checkRecorder) VerifyMetering(_ context.Context, tiers int, _ []string) error {
	c.tiers = append(c.tiers, tiers)
	return c.fail
}

func TestVerifyCatalogHoldsEachProductToItsPlan(t *testing.T) {
	plan := entitlement.CurrentPlan()
	check := &checkRecorder{}
	pay := &corebilling.Payments{Usage: check, ProductBySlug: map[string]string{plan.Slug: "prod_u"}}
	if err := verifyCatalog(t.Context(), pay); err != nil {
		t.Fatalf("verifyCatalog: %v", err)
	}
	if !slices.Equal(check.tiers, []int{plan.Tiers()}) {
		t.Fatalf("checked with tier counts %v, want [%d]", check.tiers, plan.Tiers())
	}
	check.fail = fmt.Errorf("%w: no max meter on pug.usage.t6", corebilling.ErrMeteringMisconfigured)
	if err := verifyCatalog(t.Context(), pay); !errors.Is(err, corebilling.ErrMeteringMisconfigured) {
		t.Fatalf("err = %v, want the finding returned", err)
	}
}

func newService(t *testing.T, pg *testutil.TestPostgres) *coremeter.Service {
	t.Helper()
	ents, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("entitlement service: %v", err)
	}
	return coremeter.NewService(pg.PgRO, pg.PgW, ents, &checkRecorder{}, "dodo")
}

// Another pod holding the lock is doing this work: exit 0, labelled as such.
func TestMeterUnderLockSkipsWhileTheLockIsHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- cron.WithLock(t.Context(), pg.PgW, cron.JobBillingMeter, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	outcome, err := meterUnderLock(t.Context(), pg.PgW, newService(t, pg), time.Now())
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatalf("holding the lock: %v", lockErr)
	}
	if err != nil || outcome != outcomeLockHeld {
		t.Fatalf("outcome = %q, err = %v; want lock_held and no error", outcome, err)
	}
}

func TestMeterUnderLockNamesItsOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	svc := newService(t, pg)
	if outcome, err := meterUnderLock(t.Context(), pg.PgW, svc, time.Now()); err != nil || outcome != outcomeIdle {
		t.Fatalf("with nothing to meter: outcome = %q, err = %v; want idle", outcome, err)
	}
	// A subscriber, and no usage pass has ever run: nothing is stated, and the
	// CronJob goes red under its own label.
	seedLive(t, pg)
	outcome, err := meterUnderLock(t.Context(), pg.PgW, svc, time.Now())
	if !errors.Is(err, coremeter.ErrUsageStale) || outcome != outcomeStaleUsage {
		t.Fatalf("outcome = %q, err = %v; want stale_usage", outcome, err)
	}
}
