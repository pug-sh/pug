package events

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
)

func TestProcessMessage_UnmarshalFailure(t *testing.T) {
	p := NewProcessor(nil)
	err := p.ProcessMessage(context.Background(), []byte("not-proto"))
	if err == nil {
		t.Fatal("expected error for invalid proto data, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

func TestProcessMessage_MissingProjectID(t *testing.T) {
	batch := &eventsv1.EventBatch{
		Events: []*eventsv1.Event{
			{
				EventId:    proto.String("550e8400-e29b-41d4-a716-446655440000"),
				DistinctId: proto.String("user-1"),
				Kind:       proto.String("page_view"),
				OccurTime:  timestamppb.Now(),
				SessionId:  proto.String("550e8400-e29b-41d4-a716-446655440001"),
			},
		},
		// ProjectId intentionally omitted
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	p := NewProcessor(nil)
	err = p.ProcessMessage(context.Background(), data)
	if err == nil {
		t.Fatal("expected validation error for missing project_id, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

func TestProcessMessage_EmptyBatch(t *testing.T) {
	batch := &eventsv1.EventBatch{
		ProjectId: proto.String("proj-123"),
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	p := NewProcessor(nil)
	if err := p.ProcessMessage(context.Background(), data); err != nil {
		t.Fatalf("expected nil for empty batch, got: %v", err)
	}
}
