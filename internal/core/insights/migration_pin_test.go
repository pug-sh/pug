package insights

import (
	"os"
	"regexp"
	"strings"
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

// TestMigration001VariantPrecision pins the DateTime64 precision in the events
// table's Variant declaration to match the case label in
// variantTypeToPropertyValueType. Changing the migration's precision (e.g. to
// DateTime64(6)) without updating the Go switch silently demotes every
// timestamp property to PROPERTY_VALUE_TYPE_OTHER — this test fails loud
// instead of waiting for the dashboard to go blind.
func TestMigration001VariantPrecision(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/001_create_events_table.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	got := string(data)

	// Pin the exact Variant declaration. Reordering, expanding, or changing
	// any slot type is a cross-cutting change that must be reflected in
	// propertyValueToVariant (write side, in internal/app/workers/events/variant.go),
	// the alignment test (variant_align_test.go::TestPropertyValueOneofCoverage),
	// and variantTypeToPropertyValueType (read side, in this package).
	const wantVariant = "Variant(String, Int64, Float64, Bool, DateTime64(3))"
	if !strings.Contains(got, wantVariant) {
		t.Errorf("migration 001 must contain %q — propertyValueToVariant and variantTypeToPropertyValueType are pinned to this slot order/precision", wantVariant)
	}
}

// TestMigration001VariantSlotsCoveredByGoSwitch parses the Variant slot list
// from migration 001 and asserts every slot has a mapping in
// variantTypeToPropertyValueType that is neither UNSPECIFIED nor OTHER. Pairs
// with TestMigration001VariantPrecision: that test catches migration drift
// against a frozen string; this one catches the dual-update failure where the
// migration AND its pinned constant change in lockstep but the Go switch is
// forgotten.
func TestMigration001VariantSlotsCoveredByGoSwitch(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/001_create_events_table.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// Match the first Map(String, Variant(...)) declaration. Both auto_properties
	// and custom_properties share the same shape, so picking either is fine; in
	// practice this captures auto_properties (declared first in migration 001).
	// The lazy `[^)]*` would stop at the first `)`, which would mis-match
	// parameterized inner types like `DateTime64(3)`. Use a balanced match
	// instead: any chars, then an inner `(...)` precision spec, then close.
	re := regexp.MustCompile(`Map\(String, Variant\(([^()]*(?:\([^)]*\)[^()]*)*)\)\)`)
	m := re.FindStringSubmatch(string(data))
	if len(m) != 2 {
		t.Fatalf("could not extract Variant(...) slot list from migration 001")
	}

	// Split the captured slot list on commas not inside parens.
	slots := splitSlotList(m[1])
	if len(slots) == 0 {
		t.Fatalf("extracted empty slot list from %q", m[1])
	}

	for _, slot := range slots {
		got := variantTypeToPropertyValueType(slot)
		switch got {
		case commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER:
			t.Errorf("migration slot %q maps to %v — add a case to variantTypeToPropertyValueType", slot, got)
		}
	}
}

// TestPropertyValueTypeReverseCoverage asserts every defined PropertyValueType
// (other than UNSPECIFIED and OTHER, which are sentinels) is producible from
// some input to variantTypeToPropertyValueType. Pairs with
// TestMigration001VariantSlotsCoveredByGoSwitch which catches drift in the
// other direction (migration adds slot, Go switch missing). This test catches
// the inverse: proto adds a PropertyValueType value, no MV path produces it.
func TestPropertyValueTypeReverseCoverage(t *testing.T) {
	// Inputs covering every Variant slot shipped by migration 001 (event
	// auto/custom properties) plus the primitive types JSONAllPathsWithTypes
	// emits for the profile JSON column in migration 004. Structured JSON
	// outputs (Array(...), Tuple(...), Dynamic) intentionally fall through to
	// OTHER and are exempt below.
	covered := map[commonv1.PropertyValueType]bool{}
	for _, raw := range []string{
		"String", "Int64", "Float64", "Bool", "DateTime64(3)",
	} {
		covered[variantTypeToPropertyValueType(raw)] = true
	}

	exempt := map[commonv1.PropertyValueType]bool{
		commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED: true,
		commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER:       true,
	}

	for v, name := range commonv1.PropertyValueType_name {
		pvt := commonv1.PropertyValueType(v)
		if exempt[pvt] || covered[pvt] {
			continue
		}
		t.Errorf("PropertyValueType %s is not produced by any input to variantTypeToPropertyValueType — add a case in service.go or extend the inputs list above", name)
	}
}

// TestMigration003JSONShape pins the `JSON(max_dynamic_paths = 1000)`
// declaration on the profiles.properties column. Silently relaxing the cap
// (or removing it altogether) changes how many distinct property keys can be
// stored as their own typed subcolumn before spilling into the shared
// fallback subcolumn, which in turn changes filter-path behaviour:
// `properties.k` on a spilled path no longer returns NULL for missing data
// and breaks IS_NOT_SET semantics in subtle ways. Pin so an accidental
// migration edit fails loudly.
func TestMigration003JSONShape(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/003_create_profiles.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	got := string(data)

	const wantDecl = "JSON(max_dynamic_paths = 1000)"
	if !strings.Contains(got, wantDecl) {
		t.Errorf("migration 003 must contain %q — ProfilePropertyExpr semantics depend on the spill threshold", wantDecl)
	}
}

// splitSlotList splits a comma-separated list while respecting parenthesized
// groups (e.g. `String, Int64, DateTime64(3)` → 3 entries, not 4).
func splitSlotList(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}
