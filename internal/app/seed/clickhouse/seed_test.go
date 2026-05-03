package seed

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestErrNoProjectsFoundWraps(t *testing.T) {
	err := fmt.Errorf("%w: %v", ErrNoProjectsFound, pgx.ErrNoRows)
	if !errors.Is(err, ErrNoProjectsFound) {
		t.Fatalf("expected errors.Is(err, ErrNoProjectsFound) to be true")
	}
}

func TestNonNoRowsDoesNotMatchErrNoProjectsFound(t *testing.T) {
	err := fmt.Errorf("resolve project id: %w", errors.New("relation projects does not exist"))
	if errors.Is(err, ErrNoProjectsFound) {
		t.Fatalf("expected errors.Is(err, ErrNoProjectsFound) to be false")
	}
}

func TestStringMapToVariantMap(t *testing.T) {
	got := stringMapToVariantMap(map[string]string{
		"plan":       "pro",
		"brand":      "acme",
		"attempts":   "42",
		"is_trial":   "true",
		"revenue":    "9.99",
		"created_at": "2026-05-03T12:34:56.789Z",
	})

	if len(got) != 6 {
		t.Fatalf("expected 6 entries, got %d", len(got))
	}

	assertVariant := func(key, wantType string, wantValue any) {
		v, ok := got[key]
		if !ok {
			t.Fatalf("missing key %q", key)
		}
		if gotType := v.Type(); gotType != wantType {
			t.Fatalf("expected key %q to have type %q, got %q", key, wantType, gotType)
		}
		gotValue := v.Any()
		switch want := wantValue.(type) {
		case time.Time:
			gotTime, ok := gotValue.(time.Time)
			if !ok {
				t.Fatalf("expected key %q to hold time.Time, got %T", key, gotValue)
			}
			if !gotTime.Equal(want) {
				t.Fatalf("expected key %q value %v, got %v", key, want, gotTime)
			}
		default:
			if gotValue != want {
				t.Fatalf("expected key %q value %#v, got %#v", key, want, gotValue)
			}
		}
	}

	assertVariant("plan", "String", "pro")
	assertVariant("brand", "String", "acme")
	assertVariant("attempts", "Int64", int64(42))
	assertVariant("is_trial", "Bool", true)
	assertVariant("revenue", "Float64", float64(9.99))
	assertVariant("created_at", "DateTime64(3)", time.Date(2026, 5, 3, 12, 34, 56, 789000000, time.UTC))

	if got := stringToVariant("false"); got.Type() != "Bool" {
		t.Fatalf("expected bool parse, got %q", got.Type())
	}
	if got := stringToVariant("2026-05-03 12:34:56"); got.Type() != "DateTime64(3)" {
		t.Fatalf("expected datetime parse, got %q", got.Type())
	}

	if got := stringMapToVariantMap(nil); got != nil {
		t.Fatalf("expected nil input to return nil, got %#v", got)
	}
	if got := stringMapToVariantMap(map[string]string{}); got != nil {
		t.Fatalf("expected empty input to return nil, got %#v", got)
	}
}

func TestAutoAnyMapToVariantMap(t *testing.T) {
	got := autoAnyMapToVariantMap(map[string]any{
		"$country":      "US",
		"$mobile":       true,
		"$screenWidth":  390,
		"$latitude":     37.7749,
		"$verified_bot": false,
	})

	assertVariant := func(key, wantType string, wantValue any) {
		t.Helper()
		v, ok := got[key]
		if !ok {
			t.Fatalf("missing key %q", key)
		}
		if gotType := v.Type(); gotType != wantType {
			t.Fatalf("expected key %q to have type %q, got %q", key, wantType, gotType)
		}
		if gotValue := v.Any(); gotValue != wantValue {
			t.Fatalf("expected key %q value %#v, got %#v", key, wantValue, gotValue)
		}
	}

	assertVariant("$country", "String", "US")
	assertVariant("$mobile", "Bool", true)
	assertVariant("$screenWidth", "Int64", int64(390))
	assertVariant("$latitude", "Float64", 37.7749)
	assertVariant("$verified_bot", "Bool", false)
}
