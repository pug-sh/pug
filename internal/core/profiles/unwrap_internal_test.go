package profiles

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
)

// TestUnwrapJSONProperties exercises the chcol.JSON → map[string]any conversion
// across the value shapes the driver actually emits for CH JSON columns.
// Mirrors internal/core/events/unwrap_internal_test.go in style.
func TestUnwrapJSONProperties(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_input_returns_non_nil_empty_map", func(t *testing.T) {
		got := unwrapJSONProperties(ctx, nil)
		if got == nil {
			t.Fatal("expected non-nil map, got nil")
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("empty_json_returns_non_nil_empty_map", func(t *testing.T) {
		j := chcol.NewJSON()
		got := unwrapJSONProperties(ctx, j)
		if got == nil || len(got) != 0 {
			t.Errorf("expected non-nil empty map, got %v", got)
		}
	})

	t.Run("scalar_types_unwrap_to_native_go_values", func(t *testing.T) {
		j := chcol.NewJSON()
		j.SetValueAtPath("name", chcol.NewVariantWithType("alice", "String"))
		j.SetValueAtPath("ltv", chcol.NewVariantWithType(int64(1234), "Int64"))
		j.SetValueAtPath("avg", chcol.NewVariantWithType(float64(99.5), "Float64"))
		j.SetValueAtPath("verified", chcol.NewVariantWithType(true, "Bool"))

		got := unwrapJSONProperties(ctx, j)
		want := map[string]any{
			"name":     "alice",
			"ltv":      int64(1234),
			"avg":      float64(99.5),
			"verified": true,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("int64_preserved_not_collapsed_to_float64", func(t *testing.T) {
		// Regression guard for the type-fidelity improvement: json.Unmarshal
		// would have collapsed 1234 to float64. Direct typed scan keeps it
		// as int64, matching the underlying CH storage type.
		j := chcol.NewJSON()
		j.SetValueAtPath("count", chcol.NewVariantWithType(int64(1234), "Int64"))
		got := unwrapJSONProperties(ctx, j)
		v, ok := got["count"].(int64)
		if !ok {
			t.Fatalf("expected int64, got %T (%v)", got["count"], got["count"])
		}
		if v != 1234 {
			t.Errorf("got %d, want 1234", v)
		}
	})

	t.Run("nested_objects_recurse", func(t *testing.T) {
		j := chcol.NewJSON()
		j.SetValueAtPath("address.city", chcol.NewVariantWithType("Berlin", "String"))
		j.SetValueAtPath("address.zip", chcol.NewVariantWithType("10115", "String"))
		j.SetValueAtPath("address.geo.lat", chcol.NewVariantWithType(float64(52.5), "Float64"))

		got := unwrapJSONProperties(ctx, j)
		want := map[string]any{
			"address": map[string]any{
				"city": "Berlin",
				"zip":  "10115",
				"geo": map[string]any{
					"lat": float64(52.5),
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("string_arrays_normalized_to_any_slice", func(t *testing.T) {
		// CH infers Array(Nullable(String)) and the driver delivers []*string.
		// The unwrap normalizes to []any so downstream structpb.NewStruct
		// (which has no []*string case) keeps working.
		a, b := "vip", "trial"
		j := chcol.NewJSON()
		j.SetValueAtPath("tags", chcol.NewVariantWithType([]*string{&a, &b}, "Array(Nullable(String))"))

		got := unwrapJSONProperties(ctx, j)
		want := map[string]any{
			"tags": []any{"vip", "trial"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("nullable_array_elements_become_nil", func(t *testing.T) {
		// A nil *string element passes through as a Go nil in the output
		// slice, preserving the "this slot was null" signal.
		a, c := "first", "third"
		j := chcol.NewJSON()
		j.SetValueAtPath("flags", chcol.NewVariantWithType([]*string{&a, nil, &c}, "Array(Nullable(String))"))

		got := unwrapJSONProperties(ctx, j)
		flags, ok := got["flags"].([]any)
		if !ok {
			t.Fatalf("expected []any, got %T", got["flags"])
		}
		if len(flags) != 3 || flags[0] != "first" || flags[1] != nil || flags[2] != "third" {
			t.Errorf("got %#v, want [first nil third]", flags)
		}
	})

	t.Run("empty_array_unwraps_to_empty_any_slice", func(t *testing.T) {
		j := chcol.NewJSON()
		j.SetValueAtPath("empty", chcol.NewVariantWithType([]*string{}, "Array(Nullable(String))"))

		got := unwrapJSONProperties(ctx, j)
		empty, ok := got["empty"].([]any)
		if !ok {
			t.Fatalf("expected []any, got %T", got["empty"])
		}
		if len(empty) != 0 {
			t.Errorf("expected empty slice, got %v", empty)
		}
	})

	t.Run("array_of_json_objects_recurses", func(t *testing.T) {
		// CH type Array(JSON(...)) — the driver delivers []chcol.JSON and
		// unwrap recurses element-wise so each nested map is materialized.
		c1 := chcol.NewJSON()
		c1.SetValueAtPath("email", chcol.NewVariantWithType("a@x", "String"))
		c2 := chcol.NewJSON()
		c2.SetValueAtPath("email", chcol.NewVariantWithType("b@x", "String"))

		j := chcol.NewJSON()
		j.SetValueAtPath("contacts", chcol.NewVariantWithType([]chcol.JSON{*c1, *c2}, "Array(JSON(...))"))

		got := unwrapJSONProperties(ctx, j)
		want := map[string]any{
			"contacts": []any{
				map[string]any{"email": "a@x"},
				map[string]any{"email": "b@x"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("time_normalized_to_rfc3339nano_string", func(t *testing.T) {
		// Mirrors events' unwrapPropertyMap behavior: structpb has no time.Time
		// case, so the boundary normalization avoids 500s on the activity
		// handler. The string is stable across the wire.
		when := time.Date(2026, 5, 18, 9, 30, 0, 123_000_000, time.UTC)
		j := chcol.NewJSON()
		j.SetValueAtPath("last_seen", chcol.NewVariantWithType(when, "DateTime64(3)"))

		got := unwrapJSONProperties(ctx, j)
		s, ok := got["last_seen"].(string)
		if !ok {
			t.Fatalf("expected string, got %T", got["last_seen"])
		}
		if s != "2026-05-18T09:30:00.123Z" {
			t.Errorf("got %q, want 2026-05-18T09:30:00.123Z", s)
		}
	})

	t.Run("nil_variant_paths_are_dropped_by_nestedmap", func(t *testing.T) {
		// chcol.JSON.NestedMap() actively skips Variants with .Nil() == true,
		// so a path whose CH value is NULL never appears in the output map.
		// Documents the contract: callers see "missing key" indistinguishably
		// from "key with null value" — matching CH JSON's own semantics where
		// missing paths just don't surface in JSONAllPathsWithTypes.
		j := chcol.NewJSON()
		j.SetValueAtPath("ghost", chcol.Variant{})
		j.SetValueAtPath("present", chcol.NewVariantWithType("yes", "String"))

		got := unwrapJSONProperties(ctx, j)
		if _, present := got["ghost"]; present {
			t.Errorf("expected ghost key to be dropped, got %#v", got["ghost"])
		}
		if got["present"] != "yes" {
			t.Errorf("expected present=yes, got %#v", got["present"])
		}
	})

	t.Run("unknown_variant_type_coerced_to_sentinel_string", func(t *testing.T) {
		// Any unknown inner type — exercising the default arm of the type
		// switch. The sentinel string makes drift visible in dashboard
		// payloads; the WARN log + counter make it visible to operators.
		j := chcol.NewJSON()
		j.SetValueAtPath("weird", chcol.NewVariantWithType([]int{1, 2, 3}, "Array(Int32)"))

		got := unwrapJSONProperties(ctx, j)
		s, ok := got["weird"].(string)
		if !ok {
			t.Fatalf("expected sentinel string, got %T", got["weird"])
		}
		if !strings.HasPrefix(s, "<unrecognized JSON value:") {
			t.Errorf("expected sentinel prefix, got %q", s)
		}
	})
}
