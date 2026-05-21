package delivery

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	deliveryv1 "github.com/pug-sh/pug/internal/gen/proto/shared/delivery/v1"
	"github.com/pug-sh/pug/internal/slogx"
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
		return nil, err
	}

	projectID := principal.Project.ID

	// Use the provided timestamp or default to current time
	eventTimestamp := req.Msg.GetEventTimestamp()
	if eventTimestamp == nil {
		eventTimestamp = timestamppb.Now()
	}

	// Create delivery event message for NATS
	msg := &deliveryv1.DeliveryEventMessage{
		ProjectId:      proto.String(projectID),
		CampaignId:     proto.String(req.Msg.GetCampaignId()),
		MessageId:      proto.String(req.Msg.GetMessageId()),
		DeviceId:       proto.String(req.Msg.GetDeviceId()),
		EventType:      req.Msg.GetEventType().Enum(),
		Platform:       req.Msg.GetPlatform().Enum(),
		EventTimestamp: eventTimestamp,
		Metadata:       req.Msg.GetMetadata(),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal delivery event message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	// Publish to NATS JetStream
	if _, err = s.producer.Publish(ctx, nats.DeliveryEventsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish delivery event to NATS", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("failed to process request"))
	}

	return connect.NewResponse(&deliveryv1.RecordEventResponse{
		Success:           proto.Bool(true),
		Message:           proto.String("Successfully recorded delivery event"),
		ShouldRetry:       proto.Bool(false),
		RetryAfterSeconds: proto.Int32(0),
	}), nil
}

// NewServer creates a new Delivery service server
func NewServer(producer jetstream.JetStream) *Server {
	return &Server{
		producer: producer,
	}
}
