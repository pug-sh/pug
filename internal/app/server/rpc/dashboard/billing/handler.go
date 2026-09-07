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

// Role gating is enforced by rpc.AuthzInterceptor before any handler runs, so a
// request reaching here proves the caller is a member. Not that the org still
// exists: it can be deleted between the two reads.
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
	// The stored row, for purchasable alone: a negotiated deal's product id lives
	// there and never reaches the wire.
	rec, err := s.service.StoredRecord(ctx, orgID)
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
	}
	// Absent means NO quota, which is what a disabled deployment and an
	// unresolvable plan both report. Emitting a zero here would tell every org on
	// a self-hosted install that it is over its limit.
	resp.IncludedEvents = int64Value(ent.IncludedEvents)
	resp.SubscriptionStatus = subStatusToRPC(ent.SubStatus).Enum()
	// Read from the same helpers the two session RPCs refuse on, so a button the
	// dashboard renders and a call that would fail cannot drift apart.
	resp.Purchasable = proto.Bool(s.service.Purchasable(rec))
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
	return connect.NewResponse(resp), nil
}

// The service logs and records at source, so the handler only translates.
func internalErr() error {
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// int64Value keeps an absent number absent on the wire. Both fields are wrappers
// rather than bare int64s because protoc-gen-es renders an edition-2023 singular
// scalar as a non-optional bigint, which would land "no quota" in the dashboard
// as a quota of zero.
func int64Value(v *int64) *wrapperspb.Int64Value {
	if v == nil {
		return nil
	}
	return wrapperspb.Int64(*v)
}

// CreateCheckoutSession opens a provider checkout and returns the URL. The
// request names a plan slug; the amount lives on the provider's product and pug
// stores no money at all.
func (s *Server) CreateCheckoutSession(
	ctx context.Context,
	req *connect.Request[billingv1.CreateCheckoutSessionRequest],
) (*connect.Response[billingv1.CreateCheckoutSessionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	// The buyer's own address, so the provider's form is pre-filled. Never the
	// identity: a customer is created per checkout, because one person can admin
	// two orgs and a shared customer would let attribution land on the wrong
	// tenant.
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}

	sessionID, url, err := s.service.CreateCheckoutSession(ctx, orgID, req.Msg.GetPlanSlug(), principal.Customer.Email)
	if err != nil {
		return nil, checkoutErr(err, orgID, req.Msg.GetPlanSlug())
	}
	return connect.NewResponse(&billingv1.CreateCheckoutSessionResponse{
		CheckoutUrl: proto.String(url),
		SessionId:   proto.String(sessionID),
	}), nil
}

// ConfirmCheckout verifies a returning buyer's checkout against the provider.
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

// confirmErr translates the confirm path. Every case below is a checkout that
// already took the customer's money except the last, so none may fall through to
// checkoutErr -- which answers as though no money had moved ("this plan cannot be
// purchased") and, for anything it has no case for, as "internal error".
// FailedPrecondition throughout, because the dashboard has to say a person is
// needed rather than that the page will update shortly.
func confirmErr(err error, orgID string) error {
	paid := func(reason apperr.Reason, msg string) error {
		return apperr.FailedPrecondition(reason, msg,
			apperr.Precondition(string(reason), orgID,
				"the payment succeeded; it cannot be applied automatically"))
	}
	switch {
	case errors.Is(err, corebilling.ErrCheckoutNotForOrg):
		return apperr.PermissionDenied(apperr.ReasonBillingCheckoutNotForOrg,
			"this checkout does not belong to this organization")
	case errors.Is(err, corebilling.ErrCurrencyNotSupported):
		return paid(apperr.ReasonBillingCurrencyUnsupported,
			"this subscription is billed in a currency pug does not support")
	case errors.Is(err, corebilling.ErrNotPurchasable):
		return paid(apperr.ReasonBillingProductUnmapped,
			"this subscription is for a product pug cannot match to a plan")
	case errors.Is(err, corebilling.ErrTwoLiveSubscriptions):
		return paid(apperr.ReasonBillingTwoLiveSubscriptions,
			"this organization already has a live subscription")
	case errors.Is(err, corebilling.ErrCheckoutFailed):
		// The one case where no money moved, so it says so plainly rather than
		// sending the buyer to support.
		return apperr.FailedPrecondition(apperr.ReasonBillingCheckoutFailed,
			"this checkout did not complete, and nothing has been charged",
			apperr.Precondition(string(apperr.ReasonBillingCheckoutFailed), orgID,
				"the payment did not go through"))
	}
	return checkoutErr(err, orgID, "")
}

// CreatePortalSession opens the provider's customer portal, which is where plan
// changes, card updates, invoices and cancellation live.
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

// ListPlans returns the tiers this deployment sells. Never a product id: the
// dashboard renders a buy button from `purchasable` alone.
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
			PriceCents:     int64Value(opt.PriceCents),
			Purchasable:    proto.Bool(opt.Purchasable),
			Slug:           proto.String(opt.Slug),
		})
	}
	return connect.NewResponse(&billingv1.ListPlansResponse{Plans: plans}), nil
}

// checkoutErr translates the session paths. Everything a caller can act on gets
// its own reason; a provider call that simply failed is internal, since its
// message is the provider's and must not reach an API consumer.
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
				"no product is configured for this plan"))
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
	}
	return billingv1.BillingStatus_BILLING_STATUS_UNSPECIFIED
}

// subStatusToRPC maps pug's subscription vocabulary. A stored word outside it --
// a provider state pug has no name for -- reports UNSPECIFIED, which is the same
// thing resolution does with it: not live.
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
