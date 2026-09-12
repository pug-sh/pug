package billing

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
)

// Role gating is enforced by rpc.AuthzInterceptor before any handler runs. Not that
// the org still exists: it can be deleted between the two reads.
type Server struct {
	service *corebilling.Service
}

func NewServer(service *corebilling.Service) *Server {
	if service == nil {
		panic("billing: service is nil")
	}
	return &Server{service: service}
}

func (s *Server) GetBillingStatus(
	ctx context.Context,
	req *connect.Request[billingv1.GetBillingStatusRequest],
) (*connect.Response[billingv1.GetBillingStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	ent, err := s.service.GetEntitlement(ctx, orgID, time.Now())
	if err != nil {
		if errors.Is(err, corebilling.ErrOrgNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
		}
		return nil, internalErr()
	}

	resp := &billingv1.GetBillingStatusResponse{
		BillingEnabled: proto.Bool(ent.BillingEnabled),
		Plan: &billingv1.Plan{
			Slug:        proto.String(ent.Slug),
			DisplayName: proto.String(ent.DisplayName),
			Currency:    proto.String(ent.Currency),
			PriceCents:  int64Value(ent.PriceCents),
		},
		PeriodEnd:   timestamppb.New(ent.PeriodEnd),
		PeriodStart: timestamppb.New(ent.PeriodStart),
		Status:      statusToRPC(ent.Status).Enum(),
		Chargeable:  proto.Bool(ent.Chargeable),
		RateCard:    rateCardToRPC(ent.Card),
		CustomTerms: termsToRPC(ent.Terms),
	}
	// Absent means NO quota, which a disabled deployment and an unresolvable plan both
	// report. A zero would tell every org on a self-hosted install it is over.
	resp.IncludedEvents = int64Value(ent.IncludedEvents)
	resp.RetentionDays = int64Value(ent.RetentionDays)
	resp.SubscriptionStatus = subStatusToRPC(ent.SubStatus).Enum()
	// Read from the same helpers the session RPCs refuse on, so a button the
	// dashboard renders and a call that would fail cannot drift apart.
	resp.Purchasable = proto.Bool(s.service.Purchasable())
	resp.Manageable = proto.Bool(s.service.Manageable(ctx, orgID))
	if !ent.SubPeriodEnd.IsZero() {
		resp.CurrentPeriodEnd = timestamppb.New(ent.SubPeriodEnd)
	}
	if !ent.TrialEndsAt.IsZero() {
		resp.TrialEndsAt = timestamppb.New(ent.TrialEndsAt)
	}
	if !ent.ContractEndsAt.IsZero() {
		resp.ContractEndsAt = timestamppb.New(ent.ContractEndsAt)
	}
	if !ent.NextChargeAt.IsZero() {
		resp.NextChargeAt = timestamppb.New(ent.NextChargeAt)
	}
	return connect.NewResponse(resp), nil
}

// The service logs and records at source, so the handler only translates.
func internalErr() error {
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// int64Value keeps an absent number absent on the wire: protoc-gen-es renders an
// edition-2023 singular scalar as a non-optional bigint, so "no quota" would land
// in the dashboard as a quota of zero.
func int64Value(v *int64) *wrapperspb.Int64Value {
	if v == nil {
		return nil
	}
	return wrapperspb.Int64(*v)
}

func rateCardToRPC(card *corebilling.RateCard) *billingv1.RateCard {
	if card == nil {
		return nil
	}
	tiers := make([]*billingv1.RateTier, 0, len(card.Tiers))
	for _, t := range card.Tiers {
		tiers = append(tiers, &billingv1.RateTier{
			UpToBlock:     proto.Int64(t.UpToBlock),
			CentsPerBlock: proto.Int64(t.CentsPerBlock),
		})
	}
	return &billingv1.RateCard{
		BlockEvents: proto.Int64(card.BlockEvents),
		FreeBlocks:  proto.Int64(card.FreeBlocks),
		Tiers:       tiers,
	}
}

func termsToRPC(terms *corebilling.CustomTerms) *billingv1.CustomTerms {
	if terms == nil {
		return nil
	}
	out := &billingv1.CustomTerms{}
	if terms.FlatFeeCents > 0 {
		out.FlatFeeCents = wrapperspb.Int64(terms.FlatFeeCents)
	}
	if terms.BlockRateCents > 0 {
		out.BlockRateCents = wrapperspb.Int64(terms.BlockRateCents)
		out.IncludedEvents = wrapperspb.Int64(terms.IncludedEvents)
	}
	return out
}

func linesToRPC(lines []corebilling.Line) []*billingv1.InvoiceLine {
	out := make([]*billingv1.InvoiceLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, &billingv1.InvoiceLine{
			Description:   proto.String(l.Description),
			Blocks:        proto.Int64(l.Blocks),
			CentsPerBlock: proto.Int64(l.CentsPerBlock),
			AmountCents:   proto.Int64(l.AmountCents),
		})
	}
	return out
}

// An unset theme is the client declining to say, not a light one.
func checkoutTheme(t billingv1.CheckoutTheme) corebilling.CheckoutTheme {
	switch t {
	case billingv1.CheckoutTheme_CHECKOUT_THEME_LIGHT:
		return corebilling.CheckoutThemeLight
	case billingv1.CheckoutTheme_CHECKOUT_THEME_DARK:
		return corebilling.CheckoutThemeDark
	default:
		return corebilling.CheckoutThemeAuto
	}
}

// CreateCheckoutSession opens a mandate-only checkout and returns the URL.
func (s *Server) CreateCheckoutSession(
	ctx context.Context,
	req *connect.Request[billingv1.CreateCheckoutSessionRequest],
) (*connect.Response[billingv1.CreateCheckoutSessionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	sessionID, url, err := s.service.CreateCheckoutSession(ctx, corebilling.Checkout{
		OrgID:    orgID,
		PlanSlug: req.Msg.GetPlanSlug(),
		Email:    principal.Customer.Email,
		Name:     principal.Customer.DisplayName,
		Theme:    checkoutTheme(req.Msg.GetTheme()),
	})
	if err != nil {
		return nil, checkoutErr(err, orgID, req.Msg.GetPlanSlug())
	}
	return connect.NewResponse(&billingv1.CreateCheckoutSessionResponse{
		CheckoutUrl: proto.String(url),
		SessionId:   proto.String(sessionID),
	}), nil
}

func (s *Server) ConfirmCheckout(
	ctx context.Context,
	req *connect.Request[billingv1.ConfirmCheckoutRequest],
) (*connect.Response[billingv1.ConfirmCheckoutResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	confirmed, err := s.service.ConfirmCheckout(ctx, orgID, req.Msg.GetSessionId(), time.Now())
	if err != nil {
		return nil, confirmErr(err, orgID)
	}
	return connect.NewResponse(&billingv1.ConfirmCheckoutResponse{
		Confirmed: proto.Bool(confirmed),
	}), nil
}

// confirmErr translates the confirm path. FailedPrecondition throughout: a person
// is needed, and none of these may read as though nothing had happened.
func confirmErr(err error, orgID string) error {
	unapplied := func(reason apperr.Reason, msg string) error {
		return apperr.FailedPrecondition(reason, msg,
			apperr.Precondition(string(reason), orgID,
				"the payment method was authorized; it cannot be applied automatically"))
	}
	switch {
	case errors.Is(err, corebilling.ErrCheckoutNotForOrg):
		return apperr.PermissionDenied(apperr.ReasonBillingCheckoutNotForOrg,
			"this checkout does not belong to this organization")
	case errors.Is(err, corebilling.ErrCurrencyNotSupported):
		return unapplied(apperr.ReasonBillingCurrencyUnsupported,
			"this subscription is billed in a currency pug does not support")
	case errors.Is(err, corebilling.ErrTwoLiveSubscriptions):
		return unapplied(apperr.ReasonBillingTwoLiveSubscriptions,
			"this organization already has a live subscription")
	case errors.Is(err, corebilling.ErrSubscriptionUnapplicable):
		return unapplied(apperr.ReasonBillingSubscriptionUnapplicable,
			"this subscription is in a state pug cannot record")
	case errors.Is(err, corebilling.ErrCheckoutFailed):
		return apperr.FailedPrecondition(apperr.ReasonBillingCheckoutFailed,
			"this checkout did not complete, and nothing has been charged",
			apperr.Precondition(string(apperr.ReasonBillingCheckoutFailed), orgID,
				"the payment method was not authorized"))
	}
	return checkoutErr(err, orgID, "")
}

func (s *Server) CreatePortalSession(
	ctx context.Context,
	req *connect.Request[billingv1.CreatePortalSessionRequest],
) (*connect.Response[billingv1.CreatePortalSessionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	url, err := s.service.CreatePortalSession(ctx, orgID)
	if err != nil {
		return nil, checkoutErr(err, orgID, "")
	}
	return connect.NewResponse(&billingv1.CreatePortalSessionResponse{
		PortalUrl: proto.String(url),
	}), nil
}

func (s *Server) ListPlans(
	ctx context.Context,
	req *connect.Request[billingv1.ListPlansRequest],
) (*connect.Response[billingv1.ListPlansResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	options, err := s.service.PlanOptions(ctx, orgID)
	if err != nil {
		if errors.Is(err, corebilling.ErrOrgNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
		}
		return nil, internalErr()
	}

	plans := make([]*billingv1.PlanOption, 0, len(options))
	for _, opt := range options {
		plans = append(plans, &billingv1.PlanOption{
			Currency:       proto.String(opt.Currency),
			DisplayName:    proto.String(opt.DisplayName),
			IncludedEvents: int64Value(opt.IncludedEvents),
			Purchasable:    proto.Bool(opt.Purchasable),
			RetentionDays:  int64Value(opt.RetentionDays),
			Slug:           proto.String(opt.Slug),
			RateCard:       rateCardToRPC(opt.Card),
			CustomTerms:    termsToRPC(opt.Terms),
		})
	}
	return connect.NewResponse(&billingv1.ListPlansResponse{Plans: plans}), nil
}

func (s *Server) GetUpcomingInvoice(
	ctx context.Context,
	req *connect.Request[billingv1.GetUpcomingInvoiceRequest],
) (*connect.Response[billingv1.GetUpcomingInvoiceResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	up, err := s.service.UpcomingInvoice(ctx, orgID, time.Now())
	if err != nil {
		if errors.Is(err, corebilling.ErrOrgNotFound) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
		}
		return nil, internalErr()
	}
	resp := &billingv1.GetUpcomingInvoiceResponse{
		PeriodStart: timestamppb.New(up.PeriodStart),
		PeriodEnd:   timestamppb.New(up.PeriodEnd),
		Currency:    proto.String(up.Currency),
	}
	if !up.NextChargeAt.IsZero() {
		resp.NextChargeAt = timestamppb.New(up.NextChargeAt)
	}
	if !up.UsageComputedAt.IsZero() {
		resp.UsageComputedAt = timestamppb.New(up.UsageComputedAt)
	}
	// The three usage states carried through: no count means no amount, never $0.
	if up.Counted {
		resp.Counted = proto.Bool(true)
		resp.EventCount = proto.Int64(up.EventCount)
	}
	if up.Quote != nil {
		resp.Blocks = proto.Int64(up.Quote.Blocks)
		resp.Lines = linesToRPC(up.Quote.Lines)
		resp.AmountCents = wrapperspb.Int64(up.Quote.TotalCents)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ListInvoices(
	ctx context.Context,
	req *connect.Request[billingv1.ListInvoicesRequest],
) (*connect.Response[billingv1.ListInvoicesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	invoices, err := s.service.ListInvoices(ctx, req.Msg.GetOrgId())
	if err != nil {
		return nil, internalErr()
	}
	out := make([]*billingv1.Invoice, 0, len(invoices))
	for _, inv := range invoices {
		out = append(out, invoiceToRPC(inv))
	}
	return connect.NewResponse(&billingv1.ListInvoicesResponse{Invoices: out}), nil
}

// invoiceToRPC carries everything but the error message, which is merchant-facing.
func invoiceToRPC(inv corebilling.Invoice) *billingv1.Invoice {
	out := &billingv1.Invoice{
		Id:                 proto.String(inv.ID),
		PeriodStart:        timestamppb.New(inv.PeriodStart),
		PeriodEnd:          timestamppb.New(inv.PeriodEnd),
		BilledFrom:         timestamppb.New(inv.BilledFrom),
		BilledTo:           timestamppb.New(inv.BilledTo),
		EventCount:         proto.Int64(inv.EventCount),
		Blocks:             proto.Int64(inv.Blocks),
		Lines:              linesToRPC(inv.Lines),
		AmountCents:        proto.Int64(inv.AmountCents),
		Currency:           proto.String(inv.Currency),
		Status:             invoiceStatusToRPC(inv.Status).Enum(),
		ProviderInvoiceUrl: proto.String(inv.ProviderInvoiceURL),
		CreateTime:         timestamppb.New(inv.CreateTime),
		Attempts:           proto.Int32(int32(inv.Attempts)),
	}
	if !inv.PaidAt.IsZero() {
		out.PaidAt = timestamppb.New(inv.PaidAt)
	}
	if !inv.FailedAt.IsZero() {
		out.FailedAt = timestamppb.New(inv.FailedAt)
	}
	if !inv.NextAttemptAt.IsZero() {
		out.NextAttemptAt = timestamppb.New(inv.NextAttemptAt)
	}
	return out
}

func (s *Server) RemovePaymentMethod(
	ctx context.Context,
	req *connect.Request[billingv1.RemovePaymentMethodRequest],
) (*connect.Response[billingv1.RemovePaymentMethodResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	if err := s.service.RemovePaymentMethod(ctx, orgID, time.Now()); err != nil {
		if errors.Is(err, corebilling.ErrNoMandate) {
			return nil, apperr.FailedPrecondition(apperr.ReasonBillingNoMandate,
				"this organization has no payment method to remove",
				apperr.Precondition(string(apperr.ReasonBillingNoMandate), orgID,
					"no live payment method is on file"))
		}
		if errors.Is(err, corebilling.ErrFinalPeriodUnsettled) {
			return nil, apperr.FailedPrecondition(apperr.ReasonBillingFinalPeriodUnsettled,
				"the final period could not be billed; the payment method is unchanged",
				apperr.Precondition(string(apperr.ReasonBillingFinalPeriodUnsettled), orgID,
					"the last charge has not settled"))
		}
		return nil, checkoutErr(err, orgID, "")
	}
	return connect.NewResponse(&billingv1.RemovePaymentMethodResponse{}), nil
}

// checkoutErr translates the session paths. A provider call that simply failed is
// internal: its message is the provider's and must not reach an API consumer.
func checkoutErr(err error, orgID, planSlug string) error {
	switch {
	case errors.Is(err, corebilling.ErrOrgNotFound):
		return apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
	case errors.Is(err, corebilling.ErrNoProvider):
		return apperr.Unavailable(apperr.ReasonBillingUnavailable,
			"this deployment has no payments provider configured")
	case errors.Is(err, corebilling.ErrPlanNotFound):
		return apperr.NotFound(apperr.ReasonBillingPlanNotFound, "no such plan",
			apperr.Resource("plan", planSlug))
	case errors.Is(err, corebilling.ErrNotPurchasable):
		return apperr.FailedPrecondition(apperr.ReasonBillingNotPurchasable,
			"this plan cannot be purchased",
			apperr.Precondition(string(apperr.ReasonBillingNotPurchasable), planSlug,
				"nothing is configured to check out against for this plan"))
	case errors.Is(err, corebilling.ErrNoCustomer):
		return apperr.FailedPrecondition(apperr.ReasonBillingNoCustomer,
			"this organization has no billing account yet",
			apperr.Precondition(string(apperr.ReasonBillingNoCustomer), orgID,
				"the organization has never completed a checkout"))
	}
	return internalErr()
}

func statusToRPC(s corebilling.Status) billingv1.BillingStatus {
	switch s {
	case corebilling.StatusTrialing:
		return billingv1.BillingStatus_BILLING_STATUS_TRIALING
	case corebilling.StatusActive:
		return billingv1.BillingStatus_BILLING_STATUS_ACTIVE
	case corebilling.StatusFree:
		return billingv1.BillingStatus_BILLING_STATUS_FREE
	case corebilling.StatusPastDue:
		return billingv1.BillingStatus_BILLING_STATUS_PAST_DUE
	}
	return billingv1.BillingStatus_BILLING_STATUS_UNSPECIFIED
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
	case corebilling.InvoiceVoid:
		return billingv1.InvoiceStatus_INVOICE_STATUS_VOID
	case corebilling.InvoiceRefunded:
		return billingv1.InvoiceStatus_INVOICE_STATUS_REFUNDED
	}
	return billingv1.InvoiceStatus_INVOICE_STATUS_UNSPECIFIED
}

// subStatusToRPC maps pug's subscription vocabulary. A stored word outside it
// reports UNSPECIFIED, which is what resolution does with it too: not live.
func subStatusToRPC(s corebilling.SubStatus) billingv1.SubscriptionStatus {
	switch s {
	case corebilling.SubStatusActive:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE
	case corebilling.SubStatusPastDue:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE
	case corebilling.SubStatusPaused:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAUSED
	case corebilling.SubStatusCancelled:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELLED
	case corebilling.SubStatusExpired:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_EXPIRED
	case corebilling.SubStatusFailed:
		return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_FAILED
	}
	return billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED
}
