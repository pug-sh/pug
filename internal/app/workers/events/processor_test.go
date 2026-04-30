package events

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
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

func TestPropertyValueToVariant(t *testing.T) {
	t.Run("nil_returns_nil", func(t *testing.T) {
		if got := propertyValueToVariant(nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("string_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: "hello"}}
		got := propertyValueToVariant(pv)
		s, ok := got.(string)
		if !ok {
			t.Fatalf("expected string, got %T", got)
		}
		if s != "hello" {
			t.Errorf("expected %q, got %q", "hello", s)
		}
	})

	t.Run("int_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 42}}
		got := propertyValueToVariant(pv)
		n, ok := got.(int64)
		if !ok {
			t.Fatalf("expected int64, got %T", got)
		}
		if n != 42 {
			t.Errorf("expected 42, got %d", n)
		}
	})

	t.Run("double_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 3.14}}
		got := propertyValueToVariant(pv)
		f, ok := got.(float64)
		if !ok {
			t.Fatalf("expected float64, got %T", got)
		}
		if f != 3.14 {
			t.Errorf("expected 3.14, got %f", f)
		}
	})

	t.Run("bool_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_BoolValue{BoolValue: true}}
		got := propertyValueToVariant(pv)
		b, ok := got.(bool)
		if !ok {
			t.Fatalf("expected bool, got %T", got)
		}
		if !b {
			t.Error("expected true, got false")
		}
	})

	t.Run("timestamp_value_truncated_to_millisecond", func(t *testing.T) {
		ts := time.Date(2026, 4, 30, 10, 0, 0, 123456789, time.UTC)
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_TimestampValue{TimestampValue: timestamppb.New(ts)}}
		got := propertyValueToVariant(pv)
		tt, ok := got.(time.Time)
		if !ok {
			t.Fatalf("expected time.Time, got %T", got)
		}
		// Must be truncated to millisecond precision and in UTC.
		want := ts.UTC().Truncate(time.Millisecond)
		if !tt.Equal(want) {
			t.Errorf("expected %v, got %v", want, tt)
		}
		if tt.Location() != time.UTC {
			t.Errorf("expected UTC location, got %v", tt.Location())
		}
	})
}
