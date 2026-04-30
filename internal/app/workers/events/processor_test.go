package events

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
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

// TestPropertyValueToVariant pins the proto-oneof → ClickHouse Variant slot mapping.
// Each typed case must produce a Variant whose Type() matches the column's slot
// declaration order (String, Int64, Float64, Bool, DateTime64(3)). Without explicit
// tagging the driver dispatches by reflect type and can route a float64 into the
// Int64 slot — the test pins the chType string to prevent that regression.
func TestPropertyValueToVariant(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_returns_absent_variant", func(t *testing.T) {
		got := propertyValueToVariant(ctx, nil)
		if !got.Nil() {
			t.Errorf("expected absent variant for nil input, got Type=%q value=%v", got.Type(), got.Any())
		}
	})

	t.Run("zero_value_property_returns_absent_variant", func(t *testing.T) {
		// Zero-value PropertyValue (no oneof set) is rejected by protovalidate
		// at the request boundary, but the worker's default arm should still
		// produce an absent Variant rather than crash on direct callers / drift.
		got := propertyValueToVariant(ctx, &commonv1.PropertyValue{})
		if !got.Nil() {
			t.Errorf("expected absent variant for zero-value PropertyValue, got Type=%q value=%v", got.Type(), got.Any())
		}
	})

	t.Run("string_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: "hello"}}
		got := propertyValueToVariant(ctx, pv)
		if got.Type() != "String" {
			t.Errorf("expected variant Type=String, got %q", got.Type())
		}
		if s, ok := got.Any().(string); !ok || s != "hello" {
			t.Errorf("expected underlying value %q, got %T %v", "hello", got.Any(), got.Any())
		}
	})

	t.Run("int_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 42}}
		got := propertyValueToVariant(ctx, pv)
		if got.Type() != "Int64" {
			t.Errorf("expected variant Type=Int64, got %q", got.Type())
		}
		if n, ok := got.Any().(int64); !ok || n != 42 {
			t.Errorf("expected underlying value int64(42), got %T %v", got.Any(), got.Any())
		}
	})

	t.Run("double_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 3.14}}
		got := propertyValueToVariant(ctx, pv)
		// Critical pin: without explicit Float64 tagging, the driver's reflect
		// dispatch can land 3.14 in the Int64 slot via float64→int64 conversion.
		if got.Type() != "Float64" {
			t.Errorf("expected variant Type=Float64, got %q (regression: untagged double routed to wrong slot)", got.Type())
		}
		if f, ok := got.Any().(float64); !ok || f != 3.14 {
			t.Errorf("expected underlying value 3.14, got %T %v", got.Any(), got.Any())
		}
	})

	t.Run("bool_value", func(t *testing.T) {
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_BoolValue{BoolValue: true}}
		got := propertyValueToVariant(ctx, pv)
		if got.Type() != "Bool" {
			t.Errorf("expected variant Type=Bool, got %q", got.Type())
		}
		if b, ok := got.Any().(bool); !ok || !b {
			t.Errorf("expected underlying value true, got %T %v", got.Any(), got.Any())
		}
	})

	t.Run("timestamp_value_truncated_to_millisecond", func(t *testing.T) {
		ts := time.Date(2026, 4, 30, 10, 0, 0, 123456789, time.UTC)
		pv := &commonv1.PropertyValue{Value: &commonv1.PropertyValue_TimestampValue{TimestampValue: timestamppb.New(ts)}}
		got := propertyValueToVariant(ctx, pv)
		if got.Type() != "DateTime64(3)" {
			t.Errorf("expected variant Type=DateTime64(3), got %q", got.Type())
		}
		tt, ok := got.Any().(time.Time)
		if !ok {
			t.Fatalf("expected underlying time.Time, got %T", got.Any())
		}
		want := ts.UTC().Truncate(time.Millisecond)
		if !tt.Equal(want) {
			t.Errorf("expected %v, got %v", want, tt)
		}
		if tt.Location() != time.UTC {
			t.Errorf("expected UTC location, got %v", tt.Location())
		}
	})
}

func TestCustomPropertiesToVariantMap(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_input_returns_nil", func(t *testing.T) {
		got := customPropertiesToVariantMap(ctx, nil)
		if got != nil {
			t.Errorf("expected nil for nil input, got %v", got)
		}
	})

	t.Run("empty_input_returns_nil", func(t *testing.T) {
		// Load-bearing: the driver maps a nil Go map to an empty CH Map (zero
		// entries), which is what the property_keys MV's notEmpty(custom_properties)
		// guard expects. An empty (but non-nil) map would also produce zero entries
		// but the contract is the nil shape — pin it to catch regressions like
		// returning make(map[string]chcol.Variant) for empty input.
		got := customPropertiesToVariantMap(ctx, map[string]*commonv1.PropertyValue{})
		if got != nil {
			t.Errorf("expected nil for empty input, got %v", got)
		}
	})

	t.Run("populated_input_returns_typed_map", func(t *testing.T) {
		props := map[string]*commonv1.PropertyValue{
			"plan":     {Value: &commonv1.PropertyValue_StringValue{StringValue: "pro"}},
			"revenue":  {Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 9.99}},
			"is_trial": {Value: &commonv1.PropertyValue_BoolValue{BoolValue: false}},
		}
		got := customPropertiesToVariantMap(ctx, props)
		if len(got) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(got))
		}
		// Each entry must carry an explicit Variant slot tag.
		assertVariant(t, got, "plan", "String", "pro")
		assertVariant(t, got, "revenue", "Float64", 9.99)
		assertVariant(t, got, "is_trial", "Bool", false)
	})
}

func assertVariant(t *testing.T, m map[string]chcol.Variant, key, wantType string, wantValue any) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("missing key %q in variant map", key)
		return
	}
	if v.Type() != wantType {
		t.Errorf("key %q: expected Type=%q, got %q", key, wantType, v.Type())
	}
	if v.Any() != wantValue {
		t.Errorf("key %q: expected value %v, got %v", key, wantValue, v.Any())
	}
}
