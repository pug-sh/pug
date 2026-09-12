package billing

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// The estimate carries the meter's freshness through: an unknown count is never
// rendered as $0, and a known one is priced by the same function as the invoice.
func TestGetUpcomingInvoiceCarriesFreshness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC))
	srv := newPayingServer(t, pg, true)

	upcoming := func() *billingv1.GetUpcomingInvoiceResponse {
		t.Helper()
		resp, err := srv.GetUpcomingInvoice(t.Context(), connect.NewRequest(&billingv1.GetUpcomingInvoiceRequest{OrgId: &orgID}))
		if err != nil {
			t.Fatalf("GetUpcomingInvoice: %v", err)
		}
		return resp.Msg
	}

	before := upcoming()
	if before.GetCounted() || before.GetAmountCents() != nil || before.GetUsageComputedAt() != nil {
		t.Errorf("never metered: %v, want nothing counted and no amount", before)
	}
	if before.GetNextChargeAt() == nil || before.GetPeriodStart() == nil {
		t.Error("period bounds or next charge are absent")
	}

	start := before.GetPeriodStart().AsTime()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into usage_periods (event_count, org_id, period_end, period_start, usage_computed_at)
		 values (2340000, $1, $2, $3, now())`, orgID, before.GetPeriodEnd().AsTime(), start); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	after := upcoming()
	if !after.GetCounted() || after.GetEventCount() != 2_340_000 || after.GetBlocks() != 23 {
		t.Errorf("metered: %v, want 2340000 events in 23 blocks", after)
	}
	if got := after.GetAmountCents(); got == nil || got.GetValue() != 9_700 {
		t.Errorf("amount = %v, want 9700", got)
	}
	if len(after.GetLines()) == 0 {
		t.Error("no lines on a priced estimate")
	}
}

// The ledger on the wire carries the receipt and never the merchant-facing error.
func TestListInvoices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	list := func() []*billingv1.Invoice {
		t.Helper()
		resp, err := srv.ListInvoices(t.Context(), connect.NewRequest(&billingv1.ListInvoicesRequest{OrgId: &orgID}))
		if err != nil {
			t.Fatalf("ListInvoices: %v", err)
		}
		return resp.Msg.GetInvoices()
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("invoices = %v, want none", got)
	}

	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_invoices (
		   amount_cents, billed_from, billed_to, blocks, currency, event_count, id, lines, org_id,
		   period_end, period_start, plan_slug, pricing, provider, provider_invoice_url, status,
		   last_error_code, last_error_message, usage_computed_at)
		 values (9700, '2026-05-10', '2026-06-10', 23, 'USD', 2340000, 'inv00000000000000001',
		         '[{"description":"blocks 2-23","blocks":22,"cents_per_block":500,"amount_cents":9700}]', $1,
		         '2026-06-10', '2026-05-10', $2, '{}', 'stub', 'https://pay.example/receipt/1', 'paid',
		         'DO_NOT_HONOR', 'merchant-facing text', now())`, orgID, corebilling.CurrentSlug); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	got := list()
	if len(got) != 1 {
		t.Fatalf("got %d invoices, want 1", len(got))
	}
	inv := got[0]
	if inv.GetStatus() != billingv1.InvoiceStatus_INVOICE_STATUS_PAID || inv.GetAmountCents() != 9_700 {
		t.Errorf("invoice = %s %d, want paid 9700", inv.GetStatus(), inv.GetAmountCents())
	}
	if inv.GetProviderInvoiceUrl() != "https://pay.example/receipt/1" {
		t.Errorf("receipt = %q", inv.GetProviderInvoiceUrl())
	}
	if s := inv.String(); contains(s, "merchant-facing") || contains(s, "DO_NOT_HONOR") {
		t.Errorf("the decline reached the wire: %s", s)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Removing a payment method that is not there is a precondition, named so the
// dashboard can say so; with no provider it is unavailable like every money path.
func TestRemovePaymentMethodRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	_, err := newPayingServer(t, pg, true).RemovePaymentMethod(buyerCtx(t),
		connect.NewRequest(&billingv1.RemovePaymentMethodRequest{OrgId: &orgID}))
	if err == nil {
		t.Fatal("removed a payment method that does not exist")
	}
	if ae := appErr(t, err); ae.Code() != connect.CodeFailedPrecondition || ae.Reason() != apperr.ReasonBillingNoMandate {
		t.Errorf("err = %s/%s, want FailedPrecondition/%s", ae.Code(), ae.Reason(), apperr.ReasonBillingNoMandate)
	}

	_, err = newServer(t, pg, true).RemovePaymentMethod(buyerCtx(t),
		connect.NewRequest(&billingv1.RemovePaymentMethodRequest{OrgId: &orgID}))
	if err == nil {
		t.Fatal("removed a payment method with no provider")
	}
	if ae := appErr(t, err); ae.Code() != connect.CodeUnavailable {
		t.Errorf("code = %s, want Unavailable", ae.Code())
	}
}
