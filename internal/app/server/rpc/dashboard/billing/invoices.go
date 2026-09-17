package billing

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
)

// GetUpcomingInvoice prices the current period so far. Every amount the count decides is
// left absent until the meter has counted the period, so an unknown never renders as $0.
func (s *Server) GetUpcomingInvoice(
	ctx context.Context,
	req *connect.Request[billingv1.GetUpcomingInvoiceRequest],
) (*connect.Response[billingv1.GetUpcomingInvoiceResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	upcoming, err := s.service.GetUpcomingInvoice(ctx, orgID, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, corebilling.ErrBillingDisabled):
			return nil, apperr.Unavailable(apperr.ReasonBillingUnavailable, "billing is not enabled on this deployment")
		case errors.Is(err, corebilling.ErrOrgNotFound):
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
		case errors.Is(err, corebilling.ErrPlanUnpriceable):
			return nil, apperr.FailedPrecondition(apperr.ReasonBillingPlanUnpriceable,
				"this organization's plan cannot be priced",
				apperr.Precondition(string(apperr.ReasonBillingPlanUnpriceable), orgID,
					"an operator must pin a plan pug still prices"))
		}
		return nil, internalErr()
	}

	resp := &billingv1.GetUpcomingInvoiceResponse{
		CarriedCents:    proto.Int64(upcoming.CarriedCents),
		Currency:        proto.String(upcoming.Currency),
		DeferUnderCents: proto.Int64(corebilling.DeferUnderCents),
	}
	if !upcoming.Usage.UsageComputedAt.IsZero() {
		resp.UsageComputedAt = timestamppb.New(upcoming.Usage.UsageComputedAt)
	}
	if upcoming.Usage.Counted {
		resp.Counted = proto.Bool(true)
		resp.EventCount = proto.Int64(upcoming.Quote.Events)
		resp.Lines = linesToRPC(upcoming.Quote.Lines)
		resp.UsageCents = proto.Int64(upcoming.Quote.TotalCents)
		resp.AmountCents = proto.Int64(upcoming.Quote.TotalCents + upcoming.CarriedCents)
	}
	return connect.NewResponse(resp), nil
}

// ListInvoices returns the org's ledger, newest first.
func (s *Server) ListInvoices(
	ctx context.Context,
	req *connect.Request[billingv1.ListInvoicesRequest],
) (*connect.Response[billingv1.ListInvoicesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	invoices, err := s.service.ListInvoices(ctx, req.Msg.GetOrgId())
	if err != nil {
		if errors.Is(err, corebilling.ErrBillingDisabled) {
			return nil, apperr.Unavailable(apperr.ReasonBillingUnavailable, "billing is not enabled on this deployment")
		}
		return nil, internalErr()
	}
	out := make([]*billingv1.Invoice, 0, len(invoices))
	for _, inv := range invoices {
		out = append(out, invoiceToRPC(inv))
	}
	return connect.NewResponse(&billingv1.ListInvoicesResponse{Invoices: out}), nil
}

// RemovePaymentMethod charges every period the meter has counted, then cancels the
// payment method. It refuses while a period is unbilled or a charge unsettled.
func (s *Server) RemovePaymentMethod(
	ctx context.Context,
	req *connect.Request[billingv1.RemovePaymentMethodRequest],
) (*connect.Response[billingv1.RemovePaymentMethodResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}
	// The id, not the address: erasure must not blank the ledger.
	actor := "customer " + principal.Customer.ID
	if err := s.service.RemovePaymentMethod(ctx, orgID, actor, time.Now(), corebilling.Grace); err != nil {
		return nil, removeErr(err, orgID)
	}
	return connect.NewResponse(&billingv1.RemovePaymentMethodResponse{}), nil
}

// removeErr translates the removal. ErrRemoveIncomplete has to be named here: the
// removal charges before it cancels, so checkoutErr would answer an admin whose card
// was just charged as though no money had moved.
func removeErr(err error, orgID string) error {
	switch {
	case errors.Is(err, corebilling.ErrNoMandate):
		return apperr.FailedPrecondition(apperr.ReasonBillingNoMandate,
			"this organization has no payment method to remove",
			apperr.Precondition(string(apperr.ReasonBillingNoMandate), orgID,
				"no payment method is on file"))
	case errors.Is(err, corebilling.ErrFinalPeriodUnsettled):
		return apperr.FailedPrecondition(apperr.ReasonBillingFinalPeriodUnsettled,
			"the payment method was not removed: a period is still unbilled, or a charge unsettled",
			apperr.Precondition(string(apperr.ReasonBillingFinalPeriodUnsettled), orgID,
				"retry once the meter has reached the period and every charge has settled"))
	case errors.Is(err, corebilling.ErrRemoveIncomplete):
		return apperr.FailedPrecondition(apperr.ReasonBillingRemoveIncomplete,
			"the payment method was not removed and is still on file; the final period may have been charged",
			apperr.Precondition(string(apperr.ReasonBillingRemoveIncomplete), orgID,
				"retry the removal; a charge already taken is not repeated"))
	}
	return checkoutErr(err, orgID, "")
}

func linesToRPC(lines []corebilling.Line) []*billingv1.InvoiceLine {
	out := make([]*billingv1.InvoiceLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, &billingv1.InvoiceLine{
			AmountCents:     proto.Int64(l.AmountCents),
			CentsPerMillion: proto.Int64(l.CentsPerMillion),
			Description:     proto.String(l.Description),
			Events:          proto.Int64(l.Events),
		})
	}
	return out
}

func invoiceToRPC(inv corebilling.Invoice) *billingv1.Invoice {
	out := &billingv1.Invoice{
		AmountCents:  proto.Int64(inv.AmountCents),
		BilledFrom:   timestamppb.New(inv.BilledFrom),
		BilledTo:     timestamppb.New(inv.BilledTo),
		CarriedCents: proto.Int64(inv.CarriedCents),
		CreateTime:   timestamppb.New(inv.CreateTime),
		Currency:     proto.String(inv.Currency),
		EventCount:   proto.Int64(inv.EventCount),
		Id:           proto.String(inv.ID),
		Lines:        linesToRPC(inv.Lines),
		PeriodEnd:    timestamppb.New(inv.PeriodEnd),
		PeriodStart:  timestamppb.New(inv.PeriodStart),
		Status:       invoiceStatusToRPC(inv.Status).Enum(),
		TaxCents:     int64Value(inv.TaxCents),
		UsageCents:   proto.Int64(inv.UsageCents),
	}
	if inv.CoveredBy != "" {
		out.CoveredBy = proto.String(inv.CoveredBy)
	}
	if inv.ProviderInvoiceURL != "" {
		out.ProviderInvoiceUrl = proto.String(inv.ProviderInvoiceURL)
	}
	if !inv.PaidAt.IsZero() {
		out.PaidAt = timestamppb.New(inv.PaidAt)
	}
	if !inv.NextAttemptAt.IsZero() {
		out.NextAttemptAt = timestamppb.New(inv.NextAttemptAt)
	}
	return out
}

func invoiceStatusToRPC(s corebilling.InvoiceStatus) billingv1.InvoiceStatus {
	switch s {
	case corebilling.InvoiceOpen:
		return billingv1.InvoiceStatus_INVOICE_STATUS_OPEN
	case corebilling.InvoiceCharging:
		return billingv1.InvoiceStatus_INVOICE_STATUS_CHARGING
	case corebilling.InvoiceCharged:
		return billingv1.InvoiceStatus_INVOICE_STATUS_CHARGED
	case corebilling.InvoicePaid:
		return billingv1.InvoiceStatus_INVOICE_STATUS_PAID
	case corebilling.InvoiceFailed:
		return billingv1.InvoiceStatus_INVOICE_STATUS_FAILED
	case corebilling.InvoiceUncollectible:
		return billingv1.InvoiceStatus_INVOICE_STATUS_UNCOLLECTIBLE
	case corebilling.InvoiceWaived:
		return billingv1.InvoiceStatus_INVOICE_STATUS_WAIVED
	case corebilling.InvoiceDeferred:
		return billingv1.InvoiceStatus_INVOICE_STATUS_DEFERRED
	case corebilling.InvoiceVoid:
		return billingv1.InvoiceStatus_INVOICE_STATUS_VOID
	case corebilling.InvoiceRefunded:
		return billingv1.InvoiceStatus_INVOICE_STATUS_REFUNDED
	}
	return billingv1.InvoiceStatus_INVOICE_STATUS_UNSPECIFIED
}
