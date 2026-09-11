package billing

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// Every handler would nil-panic on the first call; failing here names it.
func TestNewServerRejectsANilService(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer(nil) returned a server; every RPC on it would panic")
		}
	}()
	NewServer(nil)
}

// A caller that has gone away must not start a provider or database call.
func TestEveryRPCRefusesACancelledContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newPayingServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	ctx, cancel := context.WithCancel(buyerCtx(t))
	cancel()

	slug := corebilling.CurrentSlug
	sessionID := checkoutSessionID
	calls := map[string]func() error{
		"GetBillingStatus": func() error {
			_, err := srv.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{OrgId: &orgID}))
			return err
		},
		"CreateCheckoutSession": func() error {
			_, err := srv.CreateCheckoutSession(ctx, connect.NewRequest(
				&billingv1.CreateCheckoutSessionRequest{OrgId: &orgID, PlanSlug: &slug}))
			return err
		},
		"ConfirmCheckout": func() error {
			_, err := srv.ConfirmCheckout(ctx, connect.NewRequest(
				&billingv1.ConfirmCheckoutRequest{OrgId: &orgID, SessionId: &sessionID}))
			return err
		},
		"CreatePortalSession": func() error {
			_, err := srv.CreatePortalSession(ctx, connect.NewRequest(
				&billingv1.CreatePortalSessionRequest{OrgId: &orgID}))
			return err
		},
		"ListPlans": func() error {
			_, err := srv.ListPlans(ctx, connect.NewRequest(&billingv1.ListPlansRequest{OrgId: &orgID}))
			return err
		},
		"GetUpcomingInvoice": func() error {
			_, err := srv.GetUpcomingInvoice(ctx, connect.NewRequest(&billingv1.GetUpcomingInvoiceRequest{OrgId: &orgID}))
			return err
		},
		"ListInvoices": func() error {
			_, err := srv.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{OrgId: &orgID}))
			return err
		},
		"RemovePaymentMethod": func() error {
			_, err := srv.RemovePaymentMethod(ctx, connect.NewRequest(&billingv1.RemovePaymentMethodRequest{OrgId: &orgID}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("a cancelled request was served")
			}
			if got := connect.CodeOf(err); got != connect.CodeCanceled {
				t.Errorf("code = %s, want CANCELED", got)
			}
		})
	}
}

// It reads the org's own row for the negotiated tier, so a missing org is a 404
// rather than an empty catalog.
func TestListPlansReportsAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newPayingServer(t, pg, true)

	unknown := xid.New().String()
	_, err := srv.ListPlans(t.Context(), connect.NewRequest(&billingv1.ListPlansRequest{OrgId: &unknown}))
	ae := appErr(t, err)
	if ae.Code() != connect.CodeNotFound || ae.Reason() != apperr.ReasonOrgNotFound {
		t.Errorf("err = %s/%s, want NotFound/%s", ae.Code(), ae.Reason(), apperr.ReasonOrgNotFound)
	}
}

// What a deletion racing an open billing tab produces.
func TestSessionPathsReportAnUnknownOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newPayingServer(t, pg, true)
	unknown := xid.New().String()

	if _, err := checkout(t, srv, unknown, corebilling.CurrentSlug); err == nil {
		t.Error("CreateCheckoutSession opened a checkout for an org that does not exist")
	} else if ae := appErr(t, err); ae.Code() != connect.CodeNotFound {
		t.Errorf("CreateCheckoutSession code = %s, want NotFound", ae.Code())
	}

	_, err := srv.CreatePortalSession(t.Context(), connect.NewRequest(
		&billingv1.CreatePortalSessionRequest{OrgId: &unknown}))
	if err == nil {
		t.Fatal("CreatePortalSession opened a portal for an org that does not exist")
	}
	// No subscription row means no customer, which is what the read finds first.
	if ae := appErr(t, err); ae.Code() != connect.CodeFailedPrecondition {
		t.Errorf("CreatePortalSession code = %s, want FailedPrecondition", ae.Code())
	}
}

// Absent unless a deal is stored, never a zero timestamp.
func TestGetBillingStatusCarriesTheContractEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	if before := getStatus(t, srv, orgID); before.GetContractEndsAt() != nil {
		t.Errorf("contract_ends_at = %s with no deal stored, want absent", before.GetContractEndsAt().AsTime())
	}

	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	ends := time.Now().AddDate(1, 0, 0).UTC().Truncate(time.Second)
	if _, err := svc.SetPlan(t.Context(), orgID, "tester@localhost", corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		FlatFeeCents:   new(int64(40_000)),
		ContractEndsAt: &ends,
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	after := getStatus(t, srv, orgID)
	if after.GetContractEndsAt() == nil {
		t.Fatal("contract_ends_at is absent for an org holding a dated deal")
	}
	if got := after.GetContractEndsAt().AsTime(); !got.Equal(ends) {
		t.Errorf("contract_ends_at = %s, want %s", got, ends)
	}
}

// A status the resolver does not produce today must not reach the wire as
// whichever enum happens to be zero.
func TestStatusToRPCFallsThroughToUnspecified(t *testing.T) {
	if got := statusToRPC("some_status_added_later"); got != billingv1.BillingStatus_BILLING_STATUS_UNSPECIFIED {
		t.Errorf("an unmapped status = %s, want UNSPECIFIED", got)
	}
}
