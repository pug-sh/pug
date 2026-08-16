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
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, billingEnabled)
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

// The wire shape is where "no quota" becomes visible to a client, and it is the
// distinction this endpoint exists to get right: absent means there is no limit
// to render, never a limit of zero.
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
	if off.GetPeriodStart() == nil || off.GetPeriodEnd() == nil {
		t.Error("period bounds are missing; usage is metered whether or not billing is on")
	}

	on := getStatus(t, newServer(t, pg, true), orgID)
	if !on.GetBillingEnabled() {
		t.Error("billing_enabled is false with the switch on")
	}
	if on.GetIncludedEvents() == nil {
		t.Fatal("included_events is absent with billing on; the free floor has a quota")
	}
	if on.GetIncludedEvents().GetValue() != 10_000 {
		t.Errorf("included_events = %d, want the free floor's 10000", on.GetIncludedEvents().GetValue())
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
	if msg.GetPlan().GetSlug() != corebilling.SlugTrial {
		t.Errorf("plan = %q, want the trial tier", msg.GetPlan().GetSlug())
	}
	// A price of zero is a real price and must survive as one rather than
	// collapsing into "no price recorded".
	if msg.GetPlan().GetPriceCents() == nil {
		t.Fatal("price_cents is absent on the trial tier; free is a price, not the lack of one")
	}
	if msg.GetPlan().GetPriceCents().GetValue() != 0 {
		t.Errorf("price_cents = %d, want 0", msg.GetPlan().GetPriceCents().GetValue())
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

// Every status the resolver can produce must map to a real enum value: the
// default falls through to UNSPECIFIED, which would reach the dashboard beside a
// populated plan and quota with nothing failing.
func TestStatusToRPCCoversEveryResolvedStatus(t *testing.T) {
	// Exact values, not merely "not UNSPECIFIED": two statuses swapped would tell a
	// paying customer they are on a trial, and pass a presence-only assertion.
	want := map[corebilling.Status]billingv1.BillingStatus{
		corebilling.StatusTrialing: billingv1.BillingStatus_BILLING_STATUS_TRIALING,
		corebilling.StatusActive:   billingv1.BillingStatus_BILLING_STATUS_ACTIVE,
		corebilling.StatusFree:     billingv1.BillingStatus_BILLING_STATUS_FREE,
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
