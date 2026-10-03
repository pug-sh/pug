package billing

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/meter"
	"github.com/pug-sh/pug/internal/core/billing/subscription"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func seedOrg(t *testing.T, pg *testutil.TestPostgres, createdAt time.Time) string {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme-" + xid.New().String(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, createdAt)
	return org.ID
}

// newServerWith wires the pair as the server does: the subscription service over an
// entitlement service, which the handler reads back off it. A nil payments is the
// no-provider shape.
func newServerWith(t *testing.T, pg *testutil.TestPostgres, billingEnabled bool, payments *corebilling.Payments) *Server {
	t.Helper()
	entitlements, err := entitlement.NewService(pg.PgRO, pg.PgW, billingEnabled)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	return NewServer(subscription.NewService(pg.PgRO, pg.PgW, payments, entitlements), meter.NewReader(dbread.New(pg.PgW)))
}

func newServer(t *testing.T, pg *testutil.TestPostgres, billingEnabled bool) *Server {
	t.Helper()
	return newServerWith(t, pg, billingEnabled, nil)
}

func getStatus(t *testing.T, srv *Server, orgID string) *billingv1.GetBillingStatusResponse {
	t.Helper()
	resp, err := srv.GetBillingStatus(t.Context(), connect.NewRequest(&billingv1.GetBillingStatusRequest{
		OrgId: &orgID,
	}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	return resp.Msg
}

// The wire shape is where "no quota" becomes visible to a client, and the
// distinction this endpoint exists to get right: absent is never a zero limit.
func TestGetBillingStatusOmitsTheQuotaWhenBillingIsOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	off := getStatus(t, newServer(t, pg, false), orgID)
	if off.GetBillingEnabled() {
		t.Error("billing_enabled is true with the switch off")
	}
	if off.GetIncludedEvents() != nil {
		t.Errorf("included_events = %d with billing off, want ABSENT — a bare scalar would reach "+
			"the dashboard as 0, telling every org on a self-hosted install it is over a limit "+
			"that does not exist", off.GetIncludedEvents().GetValue())
	}
	if off.GetRetentionDays() != nil {
		t.Errorf("retention_days = %d with billing off, want ABSENT — a self-hosted install "+
			"bounds nothing", off.GetRetentionDays().GetValue())
	}
	if off.GetPeriodStart() == nil || off.GetPeriodEnd() == nil {
		t.Error("period bounds are missing; usage is metered whether or not billing is on")
	}

	on := getStatus(t, newServer(t, pg, true), orgID)
	if !on.GetBillingEnabled() {
		t.Error("billing_enabled is false with the switch on")
	}
	if on.GetIncludedEvents() == nil {
		t.Fatal("included_events is absent with billing on; free has an allowance")
	}
	if on.GetIncludedEvents().GetValue() != entitlement.CurrentPlan().FreeEvents {
		t.Errorf("included_events = %d, want the current allowance", on.GetIncludedEvents().GetValue())
	}
	if on.GetRetentionDays().GetValue() != entitlement.RetentionYearDays {
		t.Errorf("retention_days = %d, want the current plan's %d",
			on.GetRetentionDays().GetValue(), entitlement.RetentionYearDays)
	}
	if on.GetStatus() != billingv1.BillingStatus_BILLING_STATUS_FREE {
		t.Errorf("status = %s, want FREE for an org with no subscription", on.GetStatus())
	}
}

func TestGetBillingStatusReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)

	unknown := xid.New().String()
	_, err := srv.GetBillingStatus(t.Context(), connect.NewRequest(&billingv1.GetBillingStatusRequest{
		OrgId: &unknown,
	}))

	// The handler returns an *apperr.Error; ErrorInterceptor is what turns it into
	// a connect error on the wire, and it is not in this call path.
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("err = %v (%T), want an apperr with CodeNotFound", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgNotFound)
	}
}

// Every status the resolver can produce must map to a real enum value: the default
// falls through to UNSPECIFIED beside a populated plan, with nothing failing.
func TestStatusToRPCCoversEveryResolvedStatus(t *testing.T) {
	// Exact values, not merely "not UNSPECIFIED": two statuses swapped would tell a
	// paying customer they are on free, and pass a presence-only assertion.
	want := map[entitlement.Status]billingv1.BillingStatus{
		entitlement.StatusActive: billingv1.BillingStatus_BILLING_STATUS_ACTIVE,
		entitlement.StatusFree:   billingv1.BillingStatus_BILLING_STATUS_FREE,
	}
	for s, w := range want {
		if got := statusToRPC(s); got != w {
			t.Errorf("statusToRPC(%s) = %s, want %s", s, got, w)
		}
	}
	if len(want) != len(entitlement.AllStatuses()) {
		t.Errorf("the table covers %d statuses, the resolver produces %d", len(want), len(entitlement.AllStatuses()))
	}
}

func TestSubStatusToRPCCoversEveryStoredStatus(t *testing.T) {
	want := map[corebilling.SubStatus]billingv1.SubscriptionStatus{
		corebilling.SubStatusActive:    billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE,
		corebilling.SubStatusPastDue:   billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE,
		corebilling.SubStatusPaused:    billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAUSED,
		corebilling.SubStatusCancelled: billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELLED,
		corebilling.SubStatusExpired:   billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_EXPIRED,
		corebilling.SubStatusFailed:    billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_FAILED,
	}
	for s, w := range want {
		if got := subStatusToRPC(s); got != w {
			t.Errorf("subStatusToRPC(%s) = %s, want %s", s, got, w)
		}
	}
	if len(want) != len(corebilling.AllSubStatuses()) {
		t.Errorf("the table covers %d statuses, the column permits %d", len(want), len(corebilling.AllSubStatuses()))
	}
	// A provider state pug has no word for is stored verbatim and reports
	// UNSPECIFIED — the same "not live" resolution gives it.
	if got := subStatusToRPC("some_state_the_provider_added"); got != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED {
		t.Errorf("an unmapped status = %s, want UNSPECIFIED", got)
	}
}

// tier_usage is the ledger's row for the live subscription's current period: what
// the provider will bill each tier, carry included, and the last tier unbounded.
func TestGetBillingStatusReportsTheStatedTiers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	start := time.Now().UTC().Truncate(time.Hour).AddDate(0, 0, -3)
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   plan_slug, price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
		   provider_updated_at, status)
		 values ('USD', $1, $2, $3, $4, $5, 100, 'dodo', 'cus_1', 'active', 'sub_1', now(), 'active')`,
		start.AddDate(0, 1, 0), start, xid.New().String(), orgID, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	// Nothing stated yet: the tiers are absent, not zeros that read as "nothing sent".
	if got := getStatus(t, srv, orgID); len(got.GetTierUsage()) != 0 || got.GetTierUsageAsOf() != nil {
		t.Fatalf("tier_usage = %v as of %v before any statement, want none", got.GetTierUsage(), got.GetTierUsageAsOf())
	}

	plan := entitlement.CurrentPlan()
	tiers := plan.Tiers()
	own, carry := make([]int64, tiers), make([]int64, tiers)
	own[0], own[1], carry[1] = 1_900_000, 100_000, 5_000
	seedLedger := func(planSlug string, allowance int64) {
		t.Helper()
		if _, err := pg.PgW.Exec(t.Context(),
			`insert into billing_meter_periods (acked, allowance, carry_events, org_id, own_events, period_start,
			   plan_slug, provider_customer_id, provider_sub_id, stated_at, summed_through, window_end, window_start)
			 values (true, $1, $2, $3, $4, $5, $6, 'cus_1', 'sub_1', now(), $7, $8, $7)
			 on conflict (org_id, period_start) do update set allowance = excluded.allowance, plan_slug = excluded.plan_slug`,
			allowance, carry, orgID, own, start, planSlug, start.Truncate(24*time.Hour),
			start.AddDate(0, 1, 0).Truncate(24*time.Hour)); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
	seedLedger(entitlement.SlugUsage, plan.FreeEvents)
	resp := getStatus(t, srv, orgID)
	got := resp.GetTierUsage()
	if len(got) != tiers || got[0].GetEvents() != 1_900_000 || got[1].GetEvents() != 105_000 {
		t.Fatalf("tier_usage = %v, want every tier with tier 2's carry included", got)
	}
	if got[0].GetFromEvents() != plan.FreeEvents || got[0].GetUpToEvents().GetValue() != plan.TierUpTo[0] ||
		got[1].GetFromEvents() != plan.TierUpTo[0] {
		t.Errorf("tiers 1 and 2 = %v, %v; want [allowance, first bound) and [first bound, ...)", got[0], got[1])
	}
	// Unbounded is absent, never a 0 a client would read as a bound.
	if got[len(got)-1].GetUpToEvents() != nil {
		t.Error("the last tier's bound must be absent")
	}
	if resp.GetTierUsageAsOf() == nil {
		t.Error("tier_usage_as_of must be set with tier_usage")
	}

	// A deal allowing more than the first bound starts tier 1 at its allowance — the
	// one it was split under — so tier 1 holds nothing rather than a range that runs
	// backwards.
	seedLedger(entitlement.SlugUsage, 5_000_000)
	got = getStatus(t, srv, orgID).GetTierUsage()
	if got[0].GetFromEvents() != 5_000_000 || got[1].GetFromEvents() != 5_000_000 {
		t.Errorf("tiers 1 and 2 start at %d and %d, want the split's allowance, 5,000,000",
			got[0].GetFromEvents(), got[1].GetFromEvents())
	}

	// Split by a plan pug no longer knows: no bounds to describe it by, so no tiers.
	seedLedger("usage-2019-01", plan.FreeEvents)
	if resp := getStatus(t, srv, orgID); len(resp.GetTierUsage()) != 0 || resp.GetTierUsageAsOf() != nil {
		t.Fatalf("tier_usage = %v under an unknown plan, want none", resp.GetTierUsage())
	}
}

// The banner's "update your card by" date: set inside the provider's grace period,
// and absent — never a zero timestamp — outside it.
func TestGetBillingStatusReportsTheGraceDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	start := time.Now().UTC().Truncate(time.Hour).AddDate(0, 0, -3)
	deadline := time.Now().UTC().Truncate(time.Second).Add(72 * time.Hour)
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, current_period_end, current_period_start, id, org_id,
		   grace_period_ends_at, plan_slug, price_cents, provider, provider_customer_id, provider_status,
		   provider_sub_id, provider_updated_at, status)
		 values ('USD', $1, $2, $3, $4, $5, $6, 100, 'dodo', 'cus_1', 'past_due', 'sub_1', now(), 'past_due')`,
		start.AddDate(0, 1, 0), start, xid.New().String(), orgID, deadline, entitlement.SlugUsage); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	got := getStatus(t, srv, orgID)
	if got.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE {
		t.Fatalf("subscription_status = %s, want PAST_DUE", got.GetSubscriptionStatus())
	}
	if got.GetGracePeriodEndsAt() == nil || !got.GetGracePeriodEndsAt().AsTime().Equal(deadline) {
		t.Errorf("grace_period_ends_at = %v, want %s", got.GetGracePeriodEndsAt(), deadline)
	}

	if _, err := pg.PgW.Exec(t.Context(),
		`update billing_subscriptions set grace_period_ends_at = null, provider_status = 'active', status = 'active'`); err != nil {
		t.Fatalf("recover subscription: %v", err)
	}
	if got := getStatus(t, srv, orgID); got.GetGracePeriodEndsAt() != nil {
		t.Errorf("grace_period_ends_at = %v with no grace window, want absent", got.GetGracePeriodEndsAt())
	}
}
