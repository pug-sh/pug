package delivery

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	deliveryv1 "github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1"
	"github.com/fivebitsio/cotton/internal/rpc/interceptors"
	pulsarclient "github.com/fivebitsio/cotton/pkg/pulsar"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the Delivery service
type Server struct {
	pc       *pulsarclient.Client
	producer *pulsarclient.Producer
}

// RecordEvent records a delivery event and writes it to Pulsar
func (s *Server) RecordEvent(
	ctx context.Context,
	req *connect.Request[deliveryv1.RecordEventRequest],
) (*connect.Response[deliveryv1.RecordEventResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	project, err := interceptors.GetProjectFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Use the provided timestamp or default to current time
	eventTimestamp := req.Msg.GetEventTimestamp()
	if eventTimestamp == nil {
		eventTimestamp = timestamppb.Now()
	}

	// Create delivery event message
	msg := &deliveryv1.RecordEventRequest{
		ProjectId:        project.ID,
		CampaignId:       req.Msg.GetCampaignId(),
		MessageId:        req.Msg.GetMessageId(),
		SubscriptionId:   req.Msg.GetSubscriptionId(),
		EventType:        req.Msg.GetEventType(),
		Platform:         req.Msg.GetPlatform(),
		EventTimestamp:   eventTimestamp,
		Metadata:         req.Msg.GetMetadata(),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal delivery event message", slog.Any("err", err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pulsarMsg := &pulsarclient.Message{
		Payload:    data,
		Properties: map[string]string{
			"event_type":        req.Msg.GetEventType().String(),
			"platform":          req.Msg.GetPlatform().String(),
			"project_id":        project.ID,
			"campaign_id":       req.Msg.GetCampaignId(),
			"subscription_id":   req.Msg.GetSubscriptionId(),
			"message_id":        req.Msg.GetMessageId(),
			"timestamp":         time.Now().Format(time.RFC3339),
		},
		DeliverAt: nil,
	}

	if err := s.producer.Send(ctx, pulsarMsg); err != nil {
		slog.ErrorContext(ctx, "failed to publish delivery event to Pulsar", slog.Any("err", err))
		return &connect.Response[deliveryv1.RecordEventResponse]{
			Msg: &deliveryv1.RecordEventResponse{
				Success:             false,
				Message:             "Failed to record delivery event",
				ShouldRetry:         true,
				RetryAfterSeconds:   5, // Retry after 5 seconds
			},
		}, nil
	}

	return &connect.Response[deliveryv1.RecordEventResponse]{
		Msg: &deliveryv1.RecordEventResponse{
			Success:             true,
			Message:             "Successfully recorded delivery event",
			ShouldRetry:         false,
			RetryAfterSeconds:   0,
		},
	}, nil
}


// NewServer creates a new Delivery service server
func NewServer(producer *pulsarclient.Producer) *Server {
	return &Server{
		producer: producer,
	}
}