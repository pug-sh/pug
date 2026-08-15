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
//
// Holds a corebilling.Reader, not a Service: entitlements are changed by
// `pug billing` alone, so the endpoint serving them has no reachable path to a
// write.
type Server struct {
	service *corebilling.Reader
}

func NewServer(service *corebilling.Reader) *Server {
	if service == nil {
		panic("billing: reader is nil")
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
		// The service logs and records at source, so the handler only translates.
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
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
	if !ent.TrialEndsAt.IsZero() {
		resp.TrialEndsAt = timestamppb.New(ent.TrialEndsAt)
	}
	if !ent.ContractEndsAt.IsZero() {
		resp.ContractEndsAt = timestamppb.New(ent.ContractEndsAt)
	}
	return connect.NewResponse(resp), nil
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
