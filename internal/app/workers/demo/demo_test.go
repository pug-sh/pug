package demo

import (
	"context"
	"math"
	"testing"
	"time"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

// TestDecideSeedAction pins the backfill gate's boundary: a completed backfill
// (n >= SeedCount) skips, an interrupted one (0 < n < SeedCount) warns rather
// than re-running into duplicates, and an empty project backfills.
func TestDecideSeedAction(t *testing.T) {
	const seedCount = 500_000
	tests := []struct {
		name string
		n    uint64
		want seedAction
	}{
		{"empty project", 0, seedBackfill},
		{"one event (interrupted)", 1, seedWarnPartial},
		{"just below target", seedCount - 1, seedWarnPartial},
		{"exactly target", seedCount, seedSkip},
		{"above target (live traffic grew it)", seedCount + 1000, seedSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideSeedAction(tt.n, seedCount); got != tt.want {
				t.Errorf("decideSeedAction(%d, %d) = %d, want %d", tt.n, seedCount, got, tt.want)
			}
		})
	}
}

// TestEnabled pins the boolean parsing of PUG_DEMO_ENABLED, which gates whether
// `pug dev` auto-starts the demo worker. Note that only Go bool literals enable
// it: a human-friendly "yes" is intentionally treated as disabled.
func TestEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"t", true},
		{"TRUE", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"yes", false},
		{"on", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("PUG_DEMO_ENABLED", tt.val)
			if got := Enabled(); got != tt.want {
				t.Errorf("Enabled() with PUG_DEMO_ENABLED=%q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

// pvKindValue reports which PropertyValue oneof slot is set and its value, so
// the type-mapping table test can assert both in a single comparison.
func pvKindValue(pv *commonv1.PropertyValue) (string, any) {
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return "string", v.StringValue
	case *commonv1.PropertyValue_BoolValue:
		return "bool", v.BoolValue
	case *commonv1.PropertyValue_IntValue:
		return "int", v.IntValue
	case *commonv1.PropertyValue_DoubleValue:
		return "double", v.DoubleValue
	default:
		return "none", nil
	}
}

// TestToPropertyValue pins the typed-value mapping the live worker applies to
// every property before publishing — especially the non-obvious NaN/Inf guard
// (a non-finite float can't ride a DoubleValue through protojson, so it falls
// back to a string). A refactor that drops the guard or reorders the type
// switch would silently mistype a ClickHouse column; this catches it.
func TestToPropertyValue(t *testing.T) {
	tests := []struct {
		name      string
		in        any
		wantKind  string
		wantValue any
	}{
		{"string", "hi", "string", "hi"},
		{"bool", true, "bool", true},
		{"int", 7, "int", int64(7)},
		{"int64", int64(9), "int", int64(9)},
		{"finite float", 3.14, "double", 3.14},
		{"NaN to string", math.NaN(), "string", "NaN"},
		{"pos inf to string", math.Inf(1), "string", "+Inf"},
		{"neg inf to string", math.Inf(-1), "string", "-Inf"},
		{"unhandled type to string", []int{1}, "string", "[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotValue := pvKindValue(toPropertyValue(context.Background(), tt.name, tt.in))
			if gotKind != tt.wantKind || gotValue != tt.wantValue {
				t.Errorf("toPropertyValue(%#v) = %s(%v), want %s(%v)", tt.in, gotKind, gotValue, tt.wantKind, tt.wantValue)
			}
		})
	}
}

// TestToPropertyValuesEmpty pins that empty/nil property maps produce a nil
// proto map (so the field is omitted on the wire) rather than an empty map.
func TestToPropertyValuesEmpty(t *testing.T) {
	if got := toPropertyValues(context.Background(), nil); got != nil {
		t.Errorf("toPropertyValues(nil) = %v, want nil", got)
	}
	if got := toPropertyValues(context.Background(), map[string]any{}); got != nil {
		t.Errorf("toPropertyValues(empty) = %v, want nil", got)
	}
}

// TestToProtoEvent pins the LiveEvent → proto field mapping (identity fields,
// timestamp, and typed properties) so a new LiveEvent field can't silently go
// unmapped on the publish path.
func TestToProtoEvent(t *testing.T) {
	e := seed.LiveEvent{
		EventID:          "evt-1",
		DistinctID:       "user-1",
		SessionID:        "sess-1",
		Kind:             "purchase",
		OccurTime:        time.Unix(1_700_000_000, 0).UTC(),
		AutoProperties:   map[string]any{"$bot_score": 90, "$latitude": 51.5},
		CustomProperties: map[string]any{"amount": 12.5, "currency": "USD"},
	}
	pe := toProtoEvent(context.Background(), e)
	if pe.GetEventId() != e.EventID || pe.GetDistinctId() != e.DistinctID ||
		pe.GetSessionId() != e.SessionID || pe.GetKind() != e.Kind {
		t.Fatalf("identity fields mismatch: %+v", pe)
	}
	if !pe.GetOccurTime().AsTime().Equal(e.OccurTime) {
		t.Errorf("occur_time = %v, want %v", pe.GetOccurTime().AsTime(), e.OccurTime)
	}
	if got := pe.GetAutoProperties()["$bot_score"].GetIntValue(); got != 90 {
		t.Errorf("$bot_score = %d, want 90", got)
	}
	if got := pe.GetAutoProperties()["$latitude"].GetDoubleValue(); got != 51.5 {
		t.Errorf("$latitude = %v, want 51.5", got)
	}
	if got := pe.GetCustomProperties()["amount"].GetDoubleValue(); got != 12.5 {
		t.Errorf("amount = %v, want 12.5", got)
	}
	if got := pe.GetCustomProperties()["currency"].GetStringValue(); got != "USD" {
		t.Errorf("currency = %q, want USD", got)
	}
}
