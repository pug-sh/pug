package insights

import (
	"os"
	"strings"
	"testing"
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
	// propertyValueToVariant, the alignment test, and this switch case.
	const wantVariant = "Variant(String, Int64, Float64, Bool, DateTime64(3))"
	if !strings.Contains(got, wantVariant) {
		t.Errorf("migration 001 must contain %q — propertyValueToVariant and variantTypeToPropertyValueType are pinned to this slot order/precision", wantVariant)
	}

	// Independently pin the precision label, since variantTypeToPropertyValueType
	// switches on the literal string variantType() returns. If 001 ever drops
	// to DateTime64(0) or jumps to DateTime64(6), the case label must follow.
	if !strings.Contains(got, "DateTime64(3)") {
		t.Error("migration 001 must use DateTime64(3) — variantTypeToPropertyValueType pins this string")
	}
}
