package events

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	coreevents "github.com/fivebitsio/cotton/internal/core/events"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/events/v1/eventsv1connect"
	"github.com/fivebitsio/cotton/internal/geo"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
)

type Server struct {
	eventsv1connect.UnimplementedEventsServiceHandler
	publisher   *coreevents.Publisher
	geoProvider geo.Provider
}

func NewServer(producer jetstream.JetStream, geoProvider geo.Provider) *Server {
	return &Server{
		publisher:   coreevents.NewPublisher(producer),
		geoProvider: geoProvider,
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

	s.enrichGeo(req.Header(), events)

	if err := s.publisher.Publish(ctx, principal.Project.ID, events); err != nil {
		slog.ErrorContext(ctx, "failed to publish events", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to accept events"))
	}

	return connect.NewResponse(&eventsv1.BatchCreateResponse{
		Accepted: uint32(len(events)),
	}), nil
}

func (s *Server) enrichGeo(h http.Header, events []*eventsv1.Event) {
	loc := s.geoProvider.Locate(h)
	if loc.IsZero() {
		return
	}
	for _, event := range events {
		if event.AutoProperties == nil {
			event.AutoProperties = make(map[string]string)
		}
		if loc.Country != "" {
			event.AutoProperties[geo.PropCountry] = loc.Country
		}
		if loc.Region != "" {
			event.AutoProperties[geo.PropRegion] = loc.Region
		}
		if loc.City != "" {
			event.AutoProperties[geo.PropCity] = loc.City
		}
	}
}
