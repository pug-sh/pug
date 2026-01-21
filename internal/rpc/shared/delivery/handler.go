package delivery

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	deliveryv1 "github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1"
	"github.com/fivebitsio/cotton/internal/rpc"
	"github.com/fivebitsio/cotton/pkg/nats"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the Delivery service
type Server struct {
	producer jetstream.JetStream
}

// RecordEvent records a delivery event and writes it to NATS
func (s *Server) RecordEvent(
	ctx context.Context,
	req *connect.Request[deliveryv1.RecordEventRequest],
) (*connect.Response[deliveryv1.RecordEventResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	projectID := principal.Project.ID

	// Use the provided timestamp or default to current time
	eventTimestamp := req.Msg.GetEventTimestamp()
	if eventTimestamp == nil {
		eventTimestamp = timestamppb.Now()
	}

	// Create delivery event message for NATS
	msg := &deliveryv1.DeliveryEventMessage{
		ProjectId:      projectID,
		CampaignId:     req.Msg.GetCampaignId(),
		MessageId:      req.Msg.GetMessageId(),
		SubscriptionId: req.Msg.GetSubscriptionId(),
		EventType:      req.Msg.GetEventType(),
		Platform:       req.Msg.GetPlatform(),
		EventTimestamp: eventTimestamp,
		Metadata:       req.Msg.GetMetadata(),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal delivery event message", slog.Any("err", err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.DeliveryEventsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish delivery event to NATS", slog.Any("err", err))
		return &connect.Response[deliveryv1.RecordEventResponse]{
			Msg: &deliveryv1.RecordEventResponse{
				Success:           false,
				Message:           "Failed to record delivery event",
				ShouldRetry:       true,
				RetryAfterSeconds: 5, // Retry after 5 seconds
			},
		}, nil
	}

	return &connect.Response[deliveryv1.RecordEventResponse]{
		Msg: &deliveryv1.RecordEventResponse{
			Success:           true,
			Message:           "Successfully recorded delivery event",
			ShouldRetry:       false,
			RetryAfterSeconds: 0,
		},
	}, nil
}

// NewServer creates a new Delivery service server
func NewServer(producer jetstream.JetStream) *Server {
	return &Server{
		producer: producer,
	}
}
