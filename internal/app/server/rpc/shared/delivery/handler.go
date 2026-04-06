package delivery

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	deliveryv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/delivery/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
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
		DeviceId:       req.Msg.GetDeviceId(),
		EventType:      req.Msg.GetEventType(),
		Platform:       req.Msg.GetPlatform(),
		EventTimestamp: eventTimestamp,
		Metadata:       req.Msg.GetMetadata(),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal delivery event message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	// Publish to NATS JetStream
	if _, err = s.producer.Publish(ctx, nats.DeliveryEventsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish delivery event to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("failed to process request"))
	}

	return connect.NewResponse(&deliveryv1.RecordEventResponse{
		Success:           true,
		Message:           "Successfully recorded delivery event",
		ShouldRetry:       false,
		RetryAfterSeconds: 0,
	}), nil
}

// NewServer creates a new Delivery service server
func NewServer(producer jetstream.JetStream) *Server {
	return &Server{
		producer: producer,
	}
}
