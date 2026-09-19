package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/mandate"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
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

func newServer(t *testing.T, pg *testutil.TestPostgres, billingEnabled bool) *Server {
	t.Helper()
	ent, err := entitlement.NewService(pg.PgRO, pg.PgW, billingEnabled)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	svc := mandate.NewService(pg.PgRO, pg.PgW, billingEnabled, nil, ent)
	return NewServer(ent, svc)
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
		t.Fatal("included_events is absent with billing on; the card has an allowance")
	}
	if got, want := on.GetIncludedEvents().GetValue(), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("included_events = %d, want the card's %d", got, want)
	}
	if got, want := on.GetRetentionDays().GetValue(), entitlement.CurrentCard().RetentionDays; got != want {
		t.Errorf("retention_days = %d, want the card's %d", got, want)
	}
	if on.GetStatus() != billingv1.BillingStatus_BILLING_STATUS_FREE {
		t.Errorf("status = %s, want FREE for an org past its trial", on.GetStatus())
	}
}

func TestGetBillingStatusReportsATrial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, 0, -2))

	msg := getStatus(t, newServer(t, pg, true), orgID)
	if msg.GetStatus() != billingv1.BillingStatus_BILLING_STATUS_TRIALING {
		t.Errorf("status = %s two days after signup, want TRIALING", msg.GetStatus())
	}
	if msg.GetTrialEndsAt() == nil {
		t.Error("trial_ends_at is absent while trialing")
	}
	// Trialing is a STATUS; the plan is still the current card, whose allowance is
	// what the org gets.
	if msg.GetPlan().GetSlug() != entitlement.CurrentCard().Slug {
		t.Errorf("plan = %q, want the current card", msg.GetPlan().GetSlug())
	}
	// A graduated card has no single list price, so the wrapper is absent rather
	// than zero -- "$0.00" beside a usage plan would be a lie.
	if msg.GetPlan().GetPriceCents() != nil {
		t.Errorf("price_cents = %d, want absent on a usage card",
			msg.GetPlan().GetPriceCents().GetValue())
	}
	if msg.GetPlan().GetCurrency() == "" {
		t.Error("currency is empty; an amount without its unit cannot be formatted")
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
	// paying customer they are on a trial, and pass a presence-only assertion.
	want := map[entitlement.Status]billingv1.BillingStatus{
		entitlement.StatusTrialing: billingv1.BillingStatus_BILLING_STATUS_TRIALING,
		entitlement.StatusActive:   billingv1.BillingStatus_BILLING_STATUS_ACTIVE,
		entitlement.StatusFree:     billingv1.BillingStatus_BILLING_STATUS_FREE,
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
