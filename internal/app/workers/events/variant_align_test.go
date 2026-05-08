package events

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

// expectedVariantTypes pins each PropertyValue oneof field name to its
// corresponding ClickHouse Variant slot, declared in
// schema/clickhouse/migrations/001_create_events_table.sql:
//
//	Variant(String, Int64, Float64, Bool, DateTime64(3))
//
// Adding a new oneof case to property_value.proto without updating this
// table — and the matching ClickHouse schema / insights type mapping — will make
// TestPropertyValueOneofCoverage fail.
var expectedVariantTypes = map[string]string{
	"string_value":    "String",
	"int_value":       "Int64",
	"double_value":    "Float64",
	"bool_value":      "Bool",
	"timestamp_value": "DateTime64(3)",
}

// samplePropertyValues returns one minimal PropertyValue per oneof case,
// keyed by the same proto field name used in expectedVariantTypes. Updating
// one without the other is caught by TestPropertyValueOneofCoverage.
func samplePropertyValues() map[string]*commonv1.PropertyValue {
	return map[string]*commonv1.PropertyValue{
		"string_value":    {Value: &commonv1.PropertyValue_StringValue{StringValue: "x"}},
		"int_value":       {Value: &commonv1.PropertyValue_IntValue{IntValue: 1}},
		"double_value":    {Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 1.5}},
		"bool_value":      {Value: &commonv1.PropertyValue_BoolValue{BoolValue: true}},
		"timestamp_value": {Value: &commonv1.PropertyValue_TimestampValue{TimestampValue: timestamppb.New(time.Unix(0, 0).UTC())}},
	}
}

// TestPropertyValueOneofCoverage walks the proto descriptor for PropertyValue
// and asserts every oneof case (a) has an entry in expectedVariantTypes, (b)
// has a sample value, and (c) when fed through propertyValueToVariant produces
// a chcol.Variant whose slot tag matches the declared mapping. Together these
// pin the proto-oneof → ClickHouse Variant slot wiring so that adding a new
// oneof case without touching propertyValueToVariant fails CI loudly.
//
// What this test alone does NOT cover (covered elsewhere):
//   - Migration slot order/precision is pinned by TestMigration001VariantPrecision.
//   - Read-side variantTypeToPropertyValueType is pinned by
//     TestVariantTypeToPropertyValueType in internal/core/insights.
func TestPropertyValueOneofCoverage(t *testing.T) {
	descriptor := (&commonv1.PropertyValue{}).ProtoReflect().Descriptor()
	oneof := descriptor.Oneofs().ByName("value")
	if oneof == nil {
		t.Fatal("PropertyValue.value oneof not found in descriptor — proto contract changed")
	}

	samples := samplePropertyValues()
	seen := make(map[string]bool, oneof.Fields().Len())

	for i := 0; i < oneof.Fields().Len(); i++ {
		field := string(oneof.Fields().Get(i).Name())
		seen[field] = true

		wantSlot, declared := expectedVariantTypes[field]
		if !declared {
			t.Errorf("oneof case %q is missing from expectedVariantTypes — wire it through propertyValueToVariant, the migration's Variant(...), and variantTypeToPropertyValueType", field)
			continue
		}

		sample, ok := samples[field]
		if !ok {
			t.Errorf("oneof case %q missing a sample value — extend samplePropertyValues", field)
			continue
		}

		// Round-trip the sample through propertyValueToVariant and assert it
		// lands in the expected ClickHouse Variant slot. This is what locks
		// the proto-oneof → write-side translator together.
		variant, err := propertyValueToVariant(sample)
		if err != nil {
			t.Errorf("oneof case %q: propertyValueToVariant returned error: %v — extend the switch", field, err)
			continue
		}
		gotSlot := variant.Type()
		if gotSlot != wantSlot {
			t.Errorf("oneof case %q: propertyValueToVariant produced slot %q, want %q", field, gotSlot, wantSlot)
		}
	}

	for declared := range expectedVariantTypes {
		if !seen[declared] {
			t.Errorf("expectedVariantTypes lists %q but the proto oneof no longer contains a matching case — remove the stale entry", declared)
		}
	}
}

// TestPropertyValueToVariant_NilAndUnsupported pins the error-return contract
// of the translator. propertyValueToVariant must return an error (not silently
// produce an absent-variant slot) when handed a nil PropertyValue or one with
// no oneof case set. proto-validate's oneof.required makes both unreachable
// from the validated RPC ingress, so an error here represents proto-future
// drift or worker-internal bugs and should be observable.
func TestPropertyValueToVariant_NilAndUnsupported(t *testing.T) {
	if _, err := propertyValueToVariant(nil); err == nil {
		t.Error("propertyValueToVariant(nil) returned no error")
	}
	if _, err := propertyValueToVariant(&commonv1.PropertyValue{}); err == nil {
		t.Error("propertyValueToVariant(empty oneof) returned no error")
	}
}

// TestPropertyValueMapToVariantMap pins the batch-translation contract:
// well-formed entries pass through tagged with the right Variant slot, and a
// nil-or-empty PropertyValue causes the offending key to be dropped (rather
// than failing the whole row).
func TestPropertyValueMapToVariantMap(t *testing.T) {
	t.Run("nil_returns_nil", func(t *testing.T) {
		if got := propertyValueMapToVariantMap(context.Background(), "test-project", nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("empty_returns_nil", func(t *testing.T) {
		got := propertyValueMapToVariantMap(context.Background(), "test-project", map[string]*commonv1.PropertyValue{})
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("primitives_tagged_correctly", func(t *testing.T) {
		src := samplePropertyValues()
		got := propertyValueMapToVariantMap(context.Background(), "test-project", src)
		if len(got) != len(src) {
			t.Fatalf("expected %d entries, got %d", len(src), len(got))
		}
		for k, v := range got {
			wantSlot, ok := expectedVariantTypes[k]
			if !ok {
				t.Errorf("unexpected key %q in output", k)
				continue
			}
			if v.Type() != wantSlot {
				t.Errorf("key %q: got slot %q, want %q", k, v.Type(), wantSlot)
			}
		}
	})

	t.Run("nil_value_is_dropped", func(t *testing.T) {
		// Build the input map directly without the helper so the zero-cap
		// PropertyValue doesn't get filtered upstream.
		src := map[string]*commonv1.PropertyValue{
			"good":   {Value: &commonv1.PropertyValue_StringValue{StringValue: "ok"}},
			"absent": nil,
			"empty":  {},
		}
		got := propertyValueMapToVariantMap(context.Background(), "test-project", src)
		if _, ok := got["good"]; !ok {
			t.Error("expected good key to be present")
		}
		if _, ok := got["absent"]; ok {
			t.Error("expected absent (nil PropertyValue) key to be dropped")
		}
		if _, ok := got["empty"]; ok {
			t.Error("expected empty (no oneof set) key to be dropped")
		}
	})
}
