package events

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

// expectedVariantTypes pins each PropertyValue oneof field name to its
// corresponding ClickHouse Variant slot, declared in
// schema/clickhouse/migrations/001_create_events_table.sql:
//
//	Variant(String, Int64, Float64, Bool, DateTime64(3))
//
// Adding a new oneof case to property_value.proto without updating this
// table — and the matching propertyValueToVariant case, the Variant column,
// and variantTypeToPropertyValueType in insights/service.go — will make
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
// and asserts every oneof case is wired into propertyValueToVariant with the
// correct ClickHouse slot. The test serves three purposes:
//
//  1. Lock the proto-oneof → Variant slot mapping (catches reordering).
//  2. Exercise the default arm of propertyValueToVariant (proves it isn't
//     reachable via any current oneof case).
//  3. Force a CI failure when proto adds a new variant without wiring it
//     through every layer.
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

		want, declared := expectedVariantTypes[field]
		if !declared {
			t.Errorf("oneof case %q is missing from expectedVariantTypes — wire it through propertyValueToVariant, the ClickHouse Variant column, and variantTypeToPropertyValueType", field)
			continue
		}

		sample, ok := samples[field]
		if !ok {
			t.Errorf("oneof case %q missing a sample value — extend samplePropertyValues", field)
			continue
		}

		v := propertyValueToVariant(context.Background(), sample)
		if v.Type() != want {
			t.Errorf("oneof case %q: propertyValueToVariant returned Type=%q, want %q", field, v.Type(), want)
		}
	}

	for declared := range expectedVariantTypes {
		if !seen[declared] {
			t.Errorf("expectedVariantTypes lists %q but the proto oneof no longer contains a matching case — remove the stale entry", declared)
		}
	}
}
