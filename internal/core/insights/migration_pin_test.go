package insights

import (
	"os"
	"regexp"
	"strings"
	"testing"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
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

	// Match the custom_properties Variant(...) declaration. The lazy `[^)]*`
	// would stop at the first `)`, which would mis-match parameterized inner
	// types like `DateTime64(3)`. Use a balanced match instead: any chars,
	// then an inner `(...)` precision spec, then close.
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
