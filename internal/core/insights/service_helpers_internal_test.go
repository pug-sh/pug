package insights

import (
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestVariantTypeToPropertyValueType(t *testing.T) {
	cases := map[string]commonv1.PropertyValueType{
		"":              commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
		"String":        commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
		"Int64":         commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		"Float64":       commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		"Number":        commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		"Bool":          commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		"DateTime64(3)": commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
		"Object":        commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"Array":         commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"None":          commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"WhoKnows":      commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
	}
	for input, want := range cases {
		got := variantTypeToPropertyValueType(input)
		if got != want {
			t.Errorf("variantTypeToPropertyValueType(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestNormalizeAllowedTypes(t *testing.T) {
	t.Run("empty_input_returns_nil", func(t *testing.T) {
		got := normalizeAllowedTypes(nil)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
		got = normalizeAllowedTypes([]commonv1.PropertyValueType{})
		if got != nil {
			t.Errorf("expected nil for empty slice, got %v", got)
		}
	})

	t.Run("all_unspecified_returns_nil", func(t *testing.T) {
		input := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
		}
		got := normalizeAllowedTypes(input)
		if got != nil {
			t.Errorf("expected nil for all-UNSPECIFIED input, got %v", got)
		}
	})

	t.Run("single_non_zero_returns_one_element", func(t *testing.T) {
		input := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING}
		got := normalizeAllowedTypes(input)
		if len(got) != 1 || got[0] != commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING {
			t.Errorf("expected [STRING], got %v", got)
		}
	})

	t.Run("duplicates_are_deduped", func(t *testing.T) {
		input := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		}
		got := normalizeAllowedTypes(input)
		if len(got) != 1 || got[0] != commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER {
			t.Errorf("expected deduped [NUMBER], got %v", got)
		}
	})

	t.Run("mixed_input_sorted_ascending", func(t *testing.T) {
		input := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		}
		got := normalizeAllowedTypes(input)
		want := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
		}
		if len(got) != len(want) {
			t.Fatalf("expected %d elements, got %d: %v", len(want), len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("position %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("mixed_with_duplicates_deduped_and_sorted", func(t *testing.T) {
		input := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		}
		got := normalizeAllowedTypes(input)
		want := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		}
		if len(got) != len(want) {
			t.Fatalf("expected %d elements, got %d: %v", len(want), len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("position %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})
}

func TestFilterAggregateKeysByType(t *testing.T) {
	rows := []AggregateKeyMeta{
		{Key: "load_time", ValueType: "Float64"},
		{Key: "is_cached", ValueType: "Bool"},
		{Key: "plan_name", ValueType: "String"},
		{Key: "user_id", ValueType: "Int64"},
		{Key: "shipped_at", ValueType: "DateTime64(3)"},
	}

	t.Run("nil_allowed_types_returns_all", func(t *testing.T) {
		got := filterAggregateKeysByType(rows, nil)
		if len(got) != len(rows) {
			t.Errorf("expected %d rows, got %d", len(rows), len(got))
		}
	})

	t.Run("number_filter_returns_float64_and_int64", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 2 {
			t.Fatalf("expected 2 rows for NUMBER filter, got %d: %v", len(got), got)
		}
		keys := map[string]bool{}
		for _, r := range got {
			keys[r.Key] = true
		}
		if !keys["load_time"] || !keys["user_id"] {
			t.Errorf("expected load_time and user_id in NUMBER filter result, got: %v", keys)
		}
	})

	t.Run("boolean_filter_returns_only_bool", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 1 || got[0].Key != "is_cached" {
			t.Errorf("expected [is_cached] for BOOLEAN filter, got: %v", got)
		}
	})

	t.Run("datetime_filter_returns_only_datetime", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 1 || got[0].Key != "shipped_at" {
			t.Errorf("expected [shipped_at] for DATETIME filter, got: %v", got)
		}
	})

	t.Run("multiple_types_filter", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 2 {
			t.Fatalf("expected 2 rows for STRING+BOOLEAN filter, got %d: %v", len(got), got)
		}
		keys := map[string]bool{}
		for _, r := range got {
			keys[r.Key] = true
		}
		if !keys["is_cached"] || !keys["plan_name"] {
			t.Errorf("expected is_cached and plan_name, got: %v", keys)
		}
	})
}
