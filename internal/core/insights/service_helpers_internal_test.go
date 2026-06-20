package insights

import (
	"testing"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestVariantTypeToPropertyValueType(t *testing.T) {
	cases := map[string]commonv1.PropertyValueType{
		"":              commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
		"String":        commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
		"Int64":         commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
		"Float64":       commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_FLOAT,
		"Bool":          commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		"DateTime64(3)": commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
		// Structured JSON outputs surface as their CH type names (e.g.
		// "Array(Nullable(String))" for arrays). They intentionally fall
		// through to OTHER — PropertyFilter's current shape can't express
		// filters on structured values.
		"Array(Nullable(String))": commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"Tuple(String, Int64)":    commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"Dynamic":                 commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
		"WhoKnows":                commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER,
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
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
		}
		got := normalizeAllowedTypes(input)
		if len(got) != 1 || got[0] != commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER {
			t.Errorf("expected deduped [INTEGER], got %v", got)
		}
	})

	t.Run("mixed_input_sorted_ascending", func(t *testing.T) {
		input := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
		}
		got := normalizeAllowedTypes(input)
		want := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
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
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		}
		got := normalizeAllowedTypes(input)
		want := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
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

func TestMergePromotedAutoDimensions(t *testing.T) {
	t.Run("all_materialized_dims_always_present", func(t *testing.T) {
		// No rollup data at all: every promoted dimension must still surface
		// (count 0) so discovery never depends on the rollup being populated.
		got := mergePromotedAutoDimensions(nil, nil)
		gotKeys := map[string]AggregateKeyMeta{}
		for _, k := range got {
			gotKeys[k.Key] = k
		}
		for _, dim := range materializedDims {
			m, ok := gotKeys[dim]
			if !ok {
				t.Fatalf("expected promoted dim %q in merged keys", dim)
			}
			if m.ValueType != promotedAutoDimValueType {
				t.Errorf("dim %q: value_type = %q, want %q", dim, m.ValueType, promotedAutoDimValueType)
			}
			if m.Count != 0 {
				t.Errorf("dim %q: expected count 0 without rollup data, got %d", dim, m.Count)
			}
		}
	})

	t.Run("rollup_counts_applied_and_sorted_by_count", func(t *testing.T) {
		seen := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		discovered := []AggregateKeyMeta{
			{Key: "$timezone", ValueType: "String", Count: 5},
		}
		rollup := []AggregateKeyMeta{
			{Key: "$browser", ValueType: "", Count: 100, LastSeen: seen},
			{Key: "$country", ValueType: "", Count: 50, LastSeen: seen},
		}
		got := mergePromotedAutoDimensions(discovered, rollup)

		byKey := map[string]AggregateKeyMeta{}
		for _, k := range got {
			byKey[k.Key] = k
		}
		if b := byKey["$browser"]; b.Count != 100 || b.ValueType != "String" || !b.LastSeen.Equal(seen) {
			t.Errorf("$browser: got %+v, want count 100 / String / %v", b, seen)
		}
		if c := byKey["$country"]; c.Count != 50 {
			t.Errorf("$country: got count %d, want 50", c.Count)
		}
		// The map-sourced key is preserved alongside the promoted dims.
		if _, ok := byKey["$timezone"]; !ok {
			t.Error("expected map-sourced $timezone to be preserved")
		}
		// Highest count first: $browser (100) before $country (50) before $timezone (5).
		if got[0].Key != "$browser" {
			t.Errorf("expected $browser first (highest count), got %q", got[0].Key)
		}
	})

	t.Run("map_sourced_duplicate_of_promoted_dim_is_dropped", func(t *testing.T) {
		// Defensive: if ingest stripping ever regressed and a promoted dim
		// leaked into property_keys, the authoritative rollup entry must win and
		// the dim must appear exactly once.
		discovered := []AggregateKeyMeta{
			{Key: "$browser", ValueType: "String", Count: 9},
		}
		rollup := []AggregateKeyMeta{
			{Key: "$browser", ValueType: "", Count: 100},
		}
		got := mergePromotedAutoDimensions(discovered, rollup)

		n := 0
		var browser AggregateKeyMeta
		for _, k := range got {
			if k.Key == "$browser" {
				n++
				browser = k
			}
		}
		if n != 1 {
			t.Fatalf("expected $browser exactly once, got %d", n)
		}
		if browser.Count != 100 {
			t.Errorf("expected authoritative rollup count 100, got %d", browser.Count)
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

	t.Run("integer_filter_returns_only_int64", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 1 || got[0].Key != "user_id" {
			t.Errorf("expected [user_id] for INTEGER filter, got: %v", got)
		}
	})

	t.Run("float_filter_returns_only_float64", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_FLOAT}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 1 || got[0].Key != "load_time" {
			t.Errorf("expected [load_time] for FLOAT filter, got: %v", got)
		}
	})

	t.Run("integer_and_float_filter_returns_both", func(t *testing.T) {
		allowed := []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_FLOAT,
		}
		got := filterAggregateKeysByType(rows, allowed)
		if len(got) != 2 {
			t.Fatalf("expected 2 rows for INTEGER+FLOAT filter, got %d: %v", len(got), got)
		}
		keys := map[string]bool{}
		for _, r := range got {
			keys[r.Key] = true
		}
		if !keys["load_time"] || !keys["user_id"] {
			t.Errorf("expected load_time and user_id, got: %v", keys)
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
