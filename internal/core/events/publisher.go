package events

import (
	"context"
	"log/slog"

	"buf.build/go/protovalidate"

	"github.com/fivebitsio/cotton/internal/deps/nats"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Publisher publishes events to NATS for internal Cotton services.
// External SDK events go through the RPC handler instead.
type Publisher struct {
	producer jetstream.JetStream
}

func NewPublisher(producer jetstream.JetStream) *Publisher {
	return &Publisher{producer: producer}
}

func (p *Publisher) Publish(ctx context.Context, projectID string, events []*eventsv1.Event) error {
	batch := &eventsv1.EventBatch{
		ProjectId: projectID,
		Events:    events,
	}

	if err := protovalidate.Validate(batch); err != nil {
		return err
	}

	data, err := proto.Marshal(batch)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal event batch", slogx.Error(err))
		return err
	}

	_, err = p.producer.Publish(ctx, nats.EventsIngestSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish events to NATS", slogx.Error(err))
		return err
	}

	return nil
}
