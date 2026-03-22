package events_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/fivebitsio/cotton/internal/core/events"
	natsdep "github.com/fivebitsio/cotton/internal/deps/nats"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/testutil"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPublisher(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	natsTest := testutil.SetupNATS(t)
	ctx := context.Background()

	nc, err := natsgo.Connect(natsTest.URL)
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("create JetStream: %v", err)
	}

	// Create the events stream (normally done by NATS migrations).
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "events",
		Subjects: []string{"events.>"},
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}

	publisher := events.NewPublisher(js)

	t.Run("publish valid batch", func(t *testing.T) {
		err := publisher.Publish(ctx, "proj-1", []*eventsv1.Event{
			{
				EventId:    uuid.NewString(),
				DistinctId: uuid.NewString(),
				SessionId:  uuid.NewString(),
				Kind:       "page_view",
				OccurTime:  timestamppb.Now(),
			},
		})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}

		// Verify message was published by consuming it.
		cons, err := js.CreateConsumer(ctx, "events", jetstream.ConsumerConfig{
			FilterSubject: natsdep.EventsIngestSubject,
		})
		if err != nil {
			t.Fatalf("create consumer: %v", err)
		}

		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5_000_000_000))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		var received int
		for msg := range msgs.Messages() {
			var batch eventsv1.EventBatch
			if err := proto.Unmarshal(msg.Data(), &batch); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if batch.ProjectId != "proj-1" {
				t.Errorf("ProjectId = %q, want %q", batch.ProjectId, "proj-1")
			}
			if len(batch.Events) != 1 {
				t.Errorf("events count = %d, want 1", len(batch.Events))
			}
			if batch.Events[0].EventId == "" {
				t.Error("EventId should not be empty")
			}
			received++
		}
		if received != 1 {
			t.Errorf("received %d messages, want 1", received)
		}
	})

	t.Run("publish multiple events", func(t *testing.T) {
		err := publisher.Publish(ctx, "proj-2", []*eventsv1.Event{
			{EventId: uuid.NewString(), DistinctId: uuid.NewString(), SessionId: uuid.NewString(), Kind: "click", OccurTime: timestamppb.Now()},
			{EventId: uuid.NewString(), DistinctId: uuid.NewString(), SessionId: uuid.NewString(), Kind: "purchase", OccurTime: timestamppb.Now()},
		})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
	})

	t.Run("empty project ID fails validation", func(t *testing.T) {
		err := publisher.Publish(ctx, "", []*eventsv1.Event{
			{EventId: uuid.NewString(), DistinctId: uuid.NewString(), SessionId: uuid.NewString(), Kind: "test", OccurTime: timestamppb.Now()},
		})
		if err == nil {
			t.Fatal("expected validation error for empty project ID, got nil")
		}
	})
}
