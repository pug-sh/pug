package billing

import (
	"errors"
	"testing"
	"time"

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
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: billingEnabled}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
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
		t.Fatal("included_events is absent with billing on; the card has a free allowance")
	}
	if on.GetIncludedEvents().GetValue() != 100_000 {
		t.Errorf("included_events = %d, want the card's free 100000", on.GetIncludedEvents().GetValue())
	}
	if on.GetRetentionDays().GetValue() != corebilling.RetentionDays {
		t.Errorf("retention_days = %d, want the card's %d",
			on.GetRetentionDays().GetValue(), corebilling.RetentionDays)
	}
	if on.GetRateCard() == nil || on.GetChargeable() {
		t.Errorf("rate_card = %v chargeable = %v, want the card and not chargeable with no mandate", on.GetRateCard(), on.GetChargeable())
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
	// The trial is a no-charge window on the current card, not a tier of its own.
	if msg.GetPlan().GetSlug() != corebilling.CurrentSlug {
		t.Errorf("plan = %q, want the current card", msg.GetPlan().GetSlug())
	}
	if msg.GetPlan().GetPriceCents() != nil {
		t.Errorf("price_cents = %v, want absent — a card has no list price", msg.GetPlan().GetPriceCents())
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
	want := map[corebilling.Status]billingv1.BillingStatus{
		corebilling.StatusTrialing: billingv1.BillingStatus_BILLING_STATUS_TRIALING,
		corebilling.StatusActive:   billingv1.BillingStatus_BILLING_STATUS_ACTIVE,
		corebilling.StatusFree:     billingv1.BillingStatus_BILLING_STATUS_FREE,
		corebilling.StatusPastDue:  billingv1.BillingStatus_BILLING_STATUS_PAST_DUE,
	}
	for s, w := range want {
		if got := statusToRPC(s); got != w {
			t.Errorf("statusToRPC(%s) = %s, want %s", s, got, w)
		}
	}
	if len(want) != len(corebilling.AllStatuses()) {
		t.Errorf("the table covers %d statuses, the resolver produces %d", len(want), len(corebilling.AllStatuses()))
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

// Every invoice status the ledger records has a wire value.
func TestInvoiceStatusToRPCCoversEveryStatus(t *testing.T) {
	seen := map[billingv1.InvoiceStatus]bool{}
	for _, s := range corebilling.AllInvoiceStatuses() {
		got := invoiceStatusToRPC(s)
		if got == billingv1.InvoiceStatus_INVOICE_STATUS_UNSPECIFIED {
			t.Errorf("invoiceStatusToRPC(%s) = UNSPECIFIED", s)
		}
		if seen[got] {
			t.Errorf("invoiceStatusToRPC(%s) = %s, already used by another status", s, got)
		}
		seen[got] = true
	}
}
