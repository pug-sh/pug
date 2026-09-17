package billing

import (
	"context"
	"encoding/json"
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

// seedLiveMandate stands in for a completed checkout: a payment method pug can charge.
// created dates it, since only the days it was live are billed.
func seedLiveMandate(t *testing.T, pg *testutil.TestPostgres, orgID string, created time.Time) {
	t.Helper()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   create_time, currency, id, on_demand, org_id, plan_slug, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ($4, 'USD', $1, true, $2, $3, 'stub', 'cus_1', 'active', 'psub_live', now(), 'active')`,
		xid.New().String(), orgID, corebilling.CurrentCard().Slug, created); err != nil {
		t.Fatalf("seed mandate: %v", err)
	}
}

// seedDailyUsage is the grain the close bills on, and the estimate predicts it with.
func seedDailyUsage(t *testing.T, pg *testutil.TestPostgres, orgID string, day time.Time, events int64) {
	t.Helper()
	projectID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: projectID, OrgID: orgID, DisplayName: "project-" + projectID,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into usage_daily (day, event_count, org_id, project_id) values ($1::date, $2, $3, $4)`,
		day, events, orgID, projectID); err != nil {
		t.Fatalf("seed usage_daily: %v", err)
	}
}

// seedInvoice stores a closed period of 200,000 events on the current card.
func seedInvoice(t *testing.T, pg *testutil.TestPostgres, orgID, status string, from time.Time) string {
	t.Helper()
	quote := corebilling.Price(corebilling.CurrentCard(), 200_000)
	lines, err := json.Marshal(quote.Lines)
	if err != nil {
		t.Fatalf("encode lines: %v", err)
	}
	id := xid.New().String()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, currency, event_count, id, lines, next_attempt_at, org_id,
		   period_end, period_start, plan_slug, pricing, status, usage_cents, usage_computed_at)
		 values ($1, $2::date, $3::date, 'USD', 200000, $4, $5, now() + interval '3 days', $6, $3::timestamptz,
		         $2::timestamptz, $7, '{}', $8, $1, now())`,
		quote.TotalCents, from, from.AddDate(0, 1, 0), id, lines, orgID, corebilling.CurrentCard().Slug, status); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return id
}

func upcoming(t *testing.T, srv *Server, orgID string) *billingv1.GetUpcomingInvoiceResponse {
	t.Helper()
	resp, err := srv.GetUpcomingInvoice(t.Context(), connect.NewRequest(&billingv1.GetUpcomingInvoiceRequest{OrgId: &orgID}))
	if err != nil {
		t.Fatalf("GetUpcomingInvoice: %v", err)
	}
	return resp.Msg
}

// The estimate carries the meter's three states onto the wire, and every amount the count
// decides stays absent until the period is counted: a TypeScript client reads it as 0.
func TestGetUpcomingInvoiceNeverRendersAnUnknownAsZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	start := getStatus(t, srv, orgID).GetPeriodStart().AsTime()
	updateCents := func(id string, cents int64) {
		if _, err := pg.PgW.Exec(t.Context(),
			`update billing_invoices set amount_cents = $2, usage_cents = $2 where id = $1`, id, cents); err != nil {
			t.Fatalf("update invoice: %v", err)
		}
	}
	updateCents(seedInvoice(t, pg, orgID, "deferred", start.AddDate(0, -2, 0)), 150)
	seedLiveMandate(t, pg, orgID, start)
	seedDailyUsage(t, pg, orgID, start, 2_340_000)
	stamp := func(periodStart time.Time, events int64) {
		if _, err := pg.PgW.Exec(t.Context(),
			`insert into usage_periods (event_count, org_id, period_end, period_start, usage_computed_at)
			 values ($1, $2, $3, $4, now())`, events, orgID, periodStart.AddDate(0, 1, 0), periodStart); err != nil {
			t.Fatalf("stamp the meter: %v", err)
		}
	}
	unknown := func(t *testing.T, msg *billingv1.GetUpcomingInvoiceResponse) {
		t.Helper()
		if msg.Counted != nil || msg.EventCount != nil || msg.UsageCents != nil || msg.AmountCents != nil || len(msg.GetLines()) != 0 {
			t.Errorf("response = %v, want no amount before the period is counted", msg)
		}
		if msg.CarriedCents == nil || msg.GetCarriedCents() != 150 || msg.GetDeferUnderCents() != corebilling.DeferUnderCents {
			t.Errorf("carried = %v, threshold = %d, want the ledger's 150 and %d whatever the meter says",
				msg.CarriedCents, msg.GetDeferUnderCents(), corebilling.DeferUnderCents)
		}
	}

	never := upcoming(t, srv, orgID)
	unknown(t, never)
	if never.UsageComputedAt != nil {
		t.Errorf("usage_computed_at = %s before the meter ever ran, want absent", never.UsageComputedAt.AsTime())
	}

	stamp(start.AddDate(0, -1, 0), 5_000_000)
	computing := upcoming(t, srv, orgID)
	unknown(t, computing)
	if computing.UsageComputedAt == nil {
		t.Error("usage_computed_at is absent for a metered org, which reads as never metered")
	}

	stamp(start, 2_340_000)
	counted := upcoming(t, srv, orgID)
	want := corebilling.Price(corebilling.CurrentCard(), 2_340_000)
	if !counted.GetCounted() || counted.GetEventCount() != 2_340_000 || counted.GetUsageCents() != want.TotalCents ||
		counted.GetAmountCents() != want.TotalCents+150 {
		t.Errorf("counted = %v, want %d cents of usage plus the 150 carried", counted, want.TotalCents)
	}
	lines := counted.GetLines()
	if len(lines) != len(want.Lines) {
		t.Fatalf("lines = %d, want %d", len(lines), len(want.Lines))
	}
	for i, w := range want.Lines {
		if lines[i].GetEvents() != w.Events || lines[i].GetCentsPerMillion() != w.CentsPerMillion ||
			lines[i].GetAmountCents() != w.AmountCents || lines[i].GetDescription() != w.Description {
			t.Errorf("line %d = %v, want %+v", i, lines[i], w)
		}
	}
}

func TestGetUpcomingInvoiceIsUnavailableWithBillingOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	_, err := newServer(t, pg, false).GetUpcomingInvoice(t.Context(),
		connect.NewRequest(&billingv1.GetUpcomingInvoiceRequest{OrgId: &orgID}))
	if ae := appErr(t, err); ae.Code() != connect.CodeUnavailable || ae.Reason() != apperr.ReasonBillingUnavailable {
		t.Errorf("err = %s/%s, want Unavailable/BILLING_UNAVAILABLE", ae.Code(), ae.Reason())
	}
}

func listInvoices(t *testing.T, srv *Server, orgID string) ([]*billingv1.Invoice, error) {
	t.Helper()
	resp, err := srv.ListInvoices(t.Context(), connect.NewRequest(&billingv1.ListInvoicesRequest{OrgId: &orgID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetInvoices(), nil
}

// A tax not yet known is absent rather than 0, and only a charge still to come is dated.
func TestListInvoicesCarriesWhatIsKnown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	august, july := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	seedInvoice(t, pg, orgID, "open", august)
	paid := seedInvoice(t, pg, orgID, "paid", july)
	if _, err := pg.PgW.Exec(t.Context(),
		`update billing_invoices set paid_at = now(), tax_cents = 36, provider_invoice_url = 'https://pay.example/r/1'
		 where id = $1`, paid); err != nil {
		t.Fatalf("pay the invoice: %v", err)
	}

	got, err := listInvoices(t, srv, orgID)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("invoices = %d, want 2", len(got))
	}
	open, settled := got[0], got[1]
	if open.GetStatus() != billingv1.InvoiceStatus_INVOICE_STATUS_OPEN || open.TaxCents != nil || open.NextAttemptAt == nil ||
		!open.GetBilledFrom().AsTime().Equal(august) || !open.GetBilledTo().AsTime().Equal(august.AddDate(0, 1, 0)) {
		t.Errorf("open invoice = %v, want no tax yet, its charge dated, and August billed", open)
	}
	if len(open.GetLines()) != 2 || open.GetLines()[1].GetCentsPerMillion() != corebilling.CurrentCard().Tiers[0].CentsPerMillion {
		t.Errorf("lines = %v, want the free band and the first tier", open.GetLines())
	}
	if settled.GetStatus() != billingv1.InvoiceStatus_INVOICE_STATUS_PAID || settled.GetTaxCents().GetValue() != 36 ||
		settled.NextAttemptAt != nil || settled.PaidAt == nil || settled.GetProviderInvoiceUrl() != "https://pay.example/r/1" {
		t.Errorf("paid invoice = %v, want its tax, receipt and payment date, and no charge to come", settled)
	}
}

// Internal, not a short list: a customer's history with a row missing reads as complete.
func TestListInvoicesRefusesARowItCannotDecode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	id := seedInvoice(t, pg, orgID, "paid", time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
	if _, err := pg.PgW.Exec(t.Context(), `update billing_invoices set lines = '{}' where id = $1`, id); err != nil {
		t.Fatalf("corrupt the lines: %v", err)
	}

	_, err := listInvoices(t, newServer(t, pg, true), orgID)
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %s, want INTERNAL", got)
	}
}

func TestInvoiceStatusToRPCCoversEveryStoredStatus(t *testing.T) {
	want := map[corebilling.InvoiceStatus]billingv1.InvoiceStatus{
		corebilling.InvoiceOpen:          billingv1.InvoiceStatus_INVOICE_STATUS_OPEN,
		corebilling.InvoiceCharging:      billingv1.InvoiceStatus_INVOICE_STATUS_CHARGING,
		corebilling.InvoiceCharged:       billingv1.InvoiceStatus_INVOICE_STATUS_CHARGED,
		corebilling.InvoicePaid:          billingv1.InvoiceStatus_INVOICE_STATUS_PAID,
		corebilling.InvoiceFailed:        billingv1.InvoiceStatus_INVOICE_STATUS_FAILED,
		corebilling.InvoiceUncollectible: billingv1.InvoiceStatus_INVOICE_STATUS_UNCOLLECTIBLE,
		corebilling.InvoiceWaived:        billingv1.InvoiceStatus_INVOICE_STATUS_WAIVED,
		corebilling.InvoiceDeferred:      billingv1.InvoiceStatus_INVOICE_STATUS_DEFERRED,
		corebilling.InvoiceVoid:          billingv1.InvoiceStatus_INVOICE_STATUS_VOID,
		corebilling.InvoiceRefunded:      billingv1.InvoiceStatus_INVOICE_STATUS_REFUNDED,
	}
	for s, w := range want {
		if got := invoiceStatusToRPC(s); got != w {
			t.Errorf("invoiceStatusToRPC(%s) = %s, want %s", s, got, w)
		}
	}
	if len(want) != len(corebilling.AllInvoiceStatuses()) {
		t.Errorf("the table covers %d statuses, the column permits %d", len(want), len(corebilling.AllInvoiceStatuses()))
	}
}

// Absent while nothing can be charged, never a zero timestamp.
func TestGetBillingStatusCarriesTheNextCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newServer(t, pg, true)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	if got := getStatus(t, srv, orgID); got.NextChargeAt != nil {
		t.Errorf("next_charge_at = %s with no payment method, want absent", got.GetNextChargeAt().AsTime())
	}
	seedLiveMandate(t, pg, orgID, getStatus(t, srv, orgID).GetPeriodStart().AsTime())
	status := getStatus(t, srv, orgID)
	want := status.GetPeriodEnd().AsTime().Add(corebilling.Grace).AddDate(0, 0, corebilling.ChargeNoticeDays)
	if status.NextChargeAt == nil || !status.GetNextChargeAt().AsTime().Equal(want) {
		t.Errorf("next_charge_at = %v, want the current period's charge at %s", status.NextChargeAt, want)
	}
}

func removePaymentMethod(ctx context.Context, srv *Server, orgID string) error {
	_, err := srv.RemovePaymentMethod(ctx, connect.NewRequest(&billingv1.RemovePaymentMethodRequest{OrgId: &orgID}))
	return err
}

// Every refusal leaves the payment method in place, and each says why.
func TestRemovePaymentMethodTranslatesItsRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)

	cases := []struct {
		name   string
		server func() *Server
		seed   func(orgID string)
		code   connect.Code
		reason apperr.Reason
	}{
		{
			"no provider", func() *Server { return newServer(t, pg, true) }, func(string) {},
			connect.CodeUnavailable, apperr.ReasonBillingUnavailable,
		},
		{
			"no payment method", func() *Server { return newPayingServer(t, pg, true) }, func(string) {},
			connect.CodeFailedPrecondition, apperr.ReasonBillingNoMandate,
		},
		{
			"a retry still to come", func() *Server { return newPayingServer(t, pg, true) },
			func(orgID string) {
				seedLiveMandate(t, pg, orgID, time.Now().AddDate(0, 0, -1))
				seedInvoice(t, pg, orgID, "failed", time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingFinalPeriodUnsettled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
			tc.seed(orgID)
			err := removePaymentMethod(buyerCtx(t), tc.server(), orgID)
			if err == nil {
				t.Fatal("the payment method was removed, want a refusal")
			}
			if ae := appErr(t, err); ae.Code() != tc.code || ae.Reason() != tc.reason {
				t.Errorf("err = %s/%s, want %s/%s", ae.Code(), ae.Reason(), tc.code, tc.reason)
			}
		})
	}
}

// cancellingProvider cancels whatever it is asked to. The event mirrors seedLiveMandate,
// or applySubscription refuses it and the mandate is never marked cancelled here.
type cancellingProvider struct{ stubProvider }

func (cancellingProvider) CancelSubscription(_ context.Context, subID string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{
		Currency:           "USD",
		OnDemand:           true,
		ProviderCustomerID: "cus_1",
		ProviderStatus:     string(corebilling.SubStatusCancelled),
		ProviderSubID:      subID,
		Status:             corebilling.SubStatusCancelled,
	}, nil
}

// The removal is attributed to the customer who asked for it, so one must be signed in.
func TestRemovePaymentMethodCancelsForASignedInCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       cancellingProvider{},
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	srv := NewServer(svc)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	seedLiveMandate(t, pg, orgID, time.Now().AddDate(0, 0, -1))

	if err := removePaymentMethod(t.Context(), srv, orgID); appErr(t, err).Code() != connect.CodeUnauthenticated {
		t.Errorf("with no customer = %v, want Unauthenticated: the removal is attributed to one", err)
	}
	if err := removePaymentMethod(buyerCtx(t), srv, orgID); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}

	var status string
	var endedAt *time.Time
	if err := pg.PgW.QueryRow(t.Context(),
		`select status, ended_at from billing_subscriptions where org_id = $1`, orgID).
		Scan(&status, &endedAt); err != nil {
		t.Fatalf("read the mandate: %v", err)
	}
	if status != string(corebilling.SubStatusCancelled) || endedAt == nil {
		t.Errorf("mandate = %s ended %v, want it cancelled and dated", status, endedAt)
	}
	if got := getStatus(t, srv, orgID); got.GetChargeable() || got.NextChargeAt != nil {
		t.Errorf("chargeable = %v, next charge = %v, want neither once the card is gone",
			got.GetChargeable(), got.NextChargeAt)
	}
	err = removePaymentMethod(buyerCtx(t), srv, orgID)
	if ae := appErr(t, err); ae.Reason() != apperr.ReasonBillingNoMandate {
		t.Errorf("second removal = %v, want BILLING_NO_MANDATE", ae.Reason())
	}
}
