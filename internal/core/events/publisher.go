package events

import (
	"context"
	"log/slog"

	"buf.build/go/protovalidate"

	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Publisher marshals events into an EventBatch and publishes to NATS.
// Used by the RPC handler to enqueue SDK-submitted events for processing.
type Publisher struct {
	producer jetstream.JetStream
}

func NewPublisher(producer jetstream.JetStream) *Publisher {
	return &Publisher{producer: producer}
}

func (p *Publisher) Publish(ctx context.Context, projectID string, events []*eventsv1.Event) error {
	batch := &eventsv1.EventBatch{
		ProjectId: &projectID,
		Events:    events,
	}

	if err := protovalidate.Validate(batch); err != nil {
		slog.ErrorContext(ctx, "event batch validation failed", slogx.Error(err), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	data, err := proto.Marshal(batch)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal event batch", slogx.Error(err), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	if _, err = p.producer.Publish(ctx, nats.EventsIngestSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish events to NATS", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	return nil
}
