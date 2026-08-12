package ai

import (
	"context"
	"errors"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/core/assistant"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1/aidashboardsv1connect"
)

type handler struct {
	svc *assistant.Service
}

var _ aidashboardsv1connect.DashboardAssistantServiceHandler = (*handler)(nil)

// Turn authenticates via WithAssistantAuth (authn middleware), validates via
// the validate interceptor, then delegates to the service with emit =
// stream.Send. Client-facing error messages are explicit strings — sentinel
// text never leaks (CLAUDE.md rule); detection-layer logging already happened
// in the service.
func (h *handler) Turn(
	ctx context.Context,
	req *connect.Request[aidashboardsv1.TurnRequest],
	stream *connect.ServerStream[aidashboardsv1.TurnResponse],
) error {
	caller, ok := authn.GetInfo(ctx).(*Caller)
	if !ok {
		// Unreachable behind the middleware; fail closed if wiring breaks.
		return connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	err := h.svc.Turn(
		ctx,
		req.Msg.GetConversationId(),
		req.Msg.GetState().GetDraft(),
		req.Msg.GetMessage(),
		assistant.CallerCredentials{JWT: caller.JWT, ProjectID: caller.ProjectID},
		stream.Send,
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, assistant.ErrHistoryLoad):
		return connect.NewError(connect.CodeUnavailable, errors.New("conversation storage unavailable (load)"))
	case errors.Is(err, assistant.ErrHistorySave):
		return connect.NewError(connect.CodeUnavailable, errors.New("conversation storage unavailable (save)"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("assistant turn failed"))
	}
}
