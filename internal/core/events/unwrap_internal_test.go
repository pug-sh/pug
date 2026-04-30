package events

import (
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
)

// TestUnwrapCustomProperties pins the scan-side conversion of the driver's
// map[string]chcol.Variant into native Go values plus RFC3339Nano timestamp
// strings. The activity-feed and event-explorer responses round-trip these
// values through structpb.NewStruct, so any value type that structpb cannot
// encode (e.g. raw time.Time) would 500 those handlers — pin both the type
// and the format.
func TestUnwrapCustomProperties(t *testing.T) {
	t.Run("nil_input_returns_nil", func(t *testing.T) {
		if got := unwrapCustomProperties(nil); got != nil {
			t.Errorf("expected nil for nil input, got %v", got)
		}
	})

	t.Run("empty_input_returns_nil", func(t *testing.T) {
		got := unwrapCustomProperties(map[string]chcol.Variant{})
		if got != nil {
			t.Errorf("expected nil for empty input, got %v", got)
		}
	})

	t.Run("primitives_pass_through_as_native_types", func(t *testing.T) {
		raw := map[string]chcol.Variant{
			"plan":     chcol.NewVariantWithType("pro", "String"),
			"user_id":  chcol.NewVariantWithType(int64(42), "Int64"),
			"revenue":  chcol.NewVariantWithType(9.99, "Float64"),
			"is_trial": chcol.NewVariantWithType(false, "Bool"),
		}
		got := unwrapCustomProperties(raw)
		assertEqual(t, got, "plan", "pro")
		assertEqual(t, got, "user_id", int64(42))
		assertEqual(t, got, "revenue", 9.99)
		assertEqual(t, got, "is_trial", false)
	})

	t.Run("timestamp_normalised_to_rfc3339nano_utc", func(t *testing.T) {
		// Construct a value in a non-UTC zone with sub-second precision; the
		// helper must coerce to UTC and emit RFC3339Nano (the format is what
		// JSON/structpb consumers downstream rely on — pin it).
		zone := time.FixedZone("PST", -8*3600)
		ts := time.Date(2026, 4, 30, 2, 0, 0, 123456789, zone)
		raw := map[string]chcol.Variant{
			"shipped_at": chcol.NewVariantWithType(ts, "DateTime64(3)"),
		}
		got := unwrapCustomProperties(raw)
		s, ok := got["shipped_at"].(string)
		if !ok {
			t.Fatalf("expected shipped_at to be string, got %T %v", got["shipped_at"], got["shipped_at"])
		}
		want := ts.UTC().Format(time.RFC3339Nano)
		if s != want {
			t.Errorf("expected %q, got %q", want, s)
		}
	})

	t.Run("absent_variant_passes_through_as_nil", func(t *testing.T) {
		// An absent Variant (no slot set) returns nil from .Any(); the helper
		// stores nil in the output, which structpb maps to NullValue downstream.
		raw := map[string]chcol.Variant{
			"missing": {},
		}
		got := unwrapCustomProperties(raw)
		if v, ok := got["missing"]; !ok {
			t.Error("expected missing key to be present in output")
		} else if v != nil {
			t.Errorf("expected nil for absent variant, got %T %v", v, v)
		}
	})
}

func assertEqual(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("missing key %q", key)
		return
	}
	if got != want {
		t.Errorf("key %q: expected %v (%T), got %v (%T)", key, want, want, got, got)
	}
}
