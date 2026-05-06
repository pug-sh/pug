package autoprop

import (
	"context"
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestPropertyValue(t *testing.T) {
	tests := []struct {
		key       string
		value     string
		assertTag func(t *testing.T, pv *commonv1.PropertyValue)
	}{
		{PropMobile, "true", assertBool(true)},
		{PropMobile, "false", assertBool(false)},
		{PropVerifiedBot, "true", assertBool(true)},
		{PropBotScore, "99", assertInt(99)},
		{PropBotScore, "0", assertInt(0)},
		{PropScreenWidth, "390", assertInt(390)},
		{PropLatitude, "37.7749", assertDouble(37.7749)},
		{PropLongitude, "-122.4194", assertDouble(-122.4194)},
		{"$country", "US", assertString("US")},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			got := PropertyValue(context.Background(), "test-project", tt.key, tt.value)
			tt.assertTag(t, got)
		})
	}
}

// TestPropertyValue_ParseFailureFallback pins the silent-fallback semantics:
// when a known typed key's value fails the typed parse, PropertyValue returns
// a StringValue with the original input. Callers observe the failure via the
// events.property_dropped_total counter (verified separately by metric tests).
func TestPropertyValue_ParseFailureFallback(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{PropBotScore, "not-a-number"},
		{PropLatitude, "34.5°N"},
		{PropMobile, "yes"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			got := PropertyValue(context.Background(), "test-project", tt.key, tt.value)
			sv, ok := got.GetValue().(*commonv1.PropertyValue_StringValue)
			if !ok {
				t.Fatalf("expected StringValue fallback for %s=%q, got %T", tt.key, tt.value, got.GetValue())
			}
			if sv.StringValue != tt.value {
				t.Fatalf("StringValue=%q, want %q", sv.StringValue, tt.value)
			}
		})
	}
}

// TestPropertyValue_BackgroundContext verifies non-production callers can use
// context.Background() — the OTel default meter is a no-op without an
// exporter, so seed/test paths can rely on this without setup.
func TestPropertyValue_BackgroundContext(t *testing.T) {
	got := PropertyValue(context.Background(), "", PropBotScore, "not-a-number")
	if _, ok := got.GetValue().(*commonv1.PropertyValue_StringValue); !ok {
		t.Fatalf("expected StringValue fallback, got %T", got.GetValue())
	}
}

func assertBool(want bool) func(*testing.T, *commonv1.PropertyValue) {
	return func(t *testing.T, pv *commonv1.PropertyValue) {
		t.Helper()
		bv, ok := pv.GetValue().(*commonv1.PropertyValue_BoolValue)
		if !ok {
			t.Fatalf("expected BoolValue, got %T", pv.GetValue())
		}
		if bv.BoolValue != want {
			t.Fatalf("BoolValue=%v, want %v", bv.BoolValue, want)
		}
	}
}

func assertInt(want int64) func(*testing.T, *commonv1.PropertyValue) {
	return func(t *testing.T, pv *commonv1.PropertyValue) {
		t.Helper()
		iv, ok := pv.GetValue().(*commonv1.PropertyValue_IntValue)
		if !ok {
			t.Fatalf("expected IntValue, got %T", pv.GetValue())
		}
		if iv.IntValue != want {
			t.Fatalf("IntValue=%d, want %d", iv.IntValue, want)
		}
	}
}

func assertDouble(want float64) func(*testing.T, *commonv1.PropertyValue) {
	return func(t *testing.T, pv *commonv1.PropertyValue) {
		t.Helper()
		dv, ok := pv.GetValue().(*commonv1.PropertyValue_DoubleValue)
		if !ok {
			t.Fatalf("expected DoubleValue, got %T", pv.GetValue())
		}
		if dv.DoubleValue != want {
			t.Fatalf("DoubleValue=%v, want %v", dv.DoubleValue, want)
		}
	}
}

func assertString(want string) func(*testing.T, *commonv1.PropertyValue) {
	return func(t *testing.T, pv *commonv1.PropertyValue) {
		t.Helper()
		sv, ok := pv.GetValue().(*commonv1.PropertyValue_StringValue)
		if !ok {
			t.Fatalf("expected StringValue, got %T", pv.GetValue())
		}
		if sv.StringValue != want {
			t.Fatalf("StringValue=%q, want %q", sv.StringValue, want)
		}
	}
}
