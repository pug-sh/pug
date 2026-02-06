package events

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreevents "github.com/fivebitsio/cotton/internal/core/events"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
)

type Server struct {
	publisher *coreevents.Publisher
}

func NewServer(producer jetstream.JetStream) *Server {
	return &Server{
		publisher: coreevents.NewPublisher(producer),
	}
}

func (s *Server) BatchCreate(
	ctx context.Context,
	req *connect.Request[eventsv1.BatchCreateRequest],
) (*connect.Response[eventsv1.BatchCreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	events := req.Msg.GetEvents()
	if len(events) == 0 {
		return connect.NewResponse(&eventsv1.BatchCreateResponse{Accepted: 0}), nil
	}

	if err := coreevents.ValidateExternalEvents(events); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	if err := s.publisher.Publish(ctx, principal.Project.ID, events); err != nil {
		slog.ErrorContext(ctx, "failed to publish events", slogx.Error(err))
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}

	return connect.NewResponse(&eventsv1.BatchCreateResponse{
		Accepted: uint32(len(events)),
	}), nil
}
