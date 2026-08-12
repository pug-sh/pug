package usage

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	usagev1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/usage/v1"
)

// Role gating is enforced by rpc.AuthzInterceptor before any handler runs, so a
// request reaching here proves the org exists and the caller is a member.
type Server struct {
	service *coreusage.Service
}

func NewServer(service *coreusage.Service) *Server {
	if service == nil {
		panic("usage: service is nil")
	}
	return &Server{service: service}
}

func (s *Server) GetUsage(
	ctx context.Context,
	req *connect.Request[usagev1.GetUsageRequest],
) (*connect.Response[usagev1.GetUsageResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	orgID := req.Msg.GetOrgId()
	periodStart, periodEnd := coreusage.CalendarMonth(time.Now())

	usage, err := s.service.GetPeriodUsage(ctx, orgID, periodStart)
	if err != nil {
		return nil, internalErr()
	}

	from, to := periodStart, periodEnd
	if r := req.Msg.GetRange(); r != nil {
		from, to = r.GetFrom().AsTime(), r.GetTo().AsTime()
	}
	daily, err := s.service.ListDailyUsage(ctx, orgID, from, to)
	if err != nil {
		return nil, internalErr()
	}

	resp := &usagev1.GetUsageResponse{
		Daily:       dailyToRPCMsgs(daily),
		PeriodEnd:   timestamppb.New(periodEnd),
		PeriodStart: timestamppb.New(periodStart),
	}
	// The two fields carry three states between them, so a client never has to
	// derive one by comparing usage_computed_at against period_start: both absent
	// means the meter has never run; stamp alone means it is alive but has not
	// reached this period yet; both present means used_events is a real sum. A
	// count is emitted only in the last case — the placeholder zero behind the
	// second one was a number the server had no basis for.
	if !usage.UsageComputedAt.IsZero() {
		resp.UsageComputedAt = timestamppb.New(usage.UsageComputedAt)
	}
	if usage.Counted {
		resp.UsedEvents = proto.Int64(usage.EventCount)
	}
	return connect.NewResponse(resp), nil
}

func dailyToRPCMsgs(daily []coreusage.DailyUsage) []*usagev1.UsageDay {
	out := make([]*usagev1.UsageDay, 0, len(daily))
	for _, d := range daily {
		out = append(out, &usagev1.UsageDay{
			Day:        timestamppb.New(d.Day),
			EventCount: proto.Int64(d.EventCount),
			ProjectId:  proto.String(d.ProjectID),
		})
	}
	return out
}

// The service logs and records at source, so the handler only translates.
func internalErr() error {
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
