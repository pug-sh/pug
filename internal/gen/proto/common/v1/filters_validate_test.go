package commonv1_test

import (
	"testing"

	"buf.build/go/protovalidate"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

// validFilter builds a PropertyFilter and asserts it passes proto validation.
func assertValid(t *testing.T, name string, f *commonv1.PropertyFilter) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := protovalidate.Validate(f); err != nil {
			t.Errorf("expected valid, got error: %v", err)
		}
	})
}

// assertInvalid builds a PropertyFilter and asserts it fails proto validation.
func assertInvalid(t *testing.T, name string, f *commonv1.PropertyFilter) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := protovalidate.Validate(f); err == nil {
			t.Error("expected validation error, got nil")
		}
	})
}

// TestPropertyFilterValidation covers all CEL rules on PropertyFilter.
// Focus is on the values_not_allowed rule that was previously inverted —
// it was incorrectly requiring values[] to be empty for IN/NOT_IN/BETWEEN/NOT_BETWEEN.
func TestPropertyFilterValidation(t *testing.T) {
	// --- values_not_allowed regression: operators that use values[] must be accepted ---

	assertValid(t, "IN_with_values", &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN,
		Values:   []string{"US", "UK"},
	})
	assertValid(t, "NOT_IN_with_values", &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN,
		Values:   []string{"CN"},
	})
	assertValid(t, "BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
		Values:   []string{"10", "50"},
	})
	assertValid(t, "NOT_BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: "amount",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
		Values:   []string{"0", "100"},
	})

	// --- values_not_allowed: operators that don't use values[] must reject non-empty values[] ---

	assertInvalid(t, "EQUALS_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
		Value:    "US",
		Values:   []string{"extra"},
	})
	assertInvalid(t, "GTE_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		Value:    "5",
		Values:   []string{"1", "2"},
	})

	// --- value_required: operators that use values[] must not require a singular value ---

	assertValid(t, "BETWEEN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: "price",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
		Values:   []string{"1", "9"},
	})
	assertValid(t, "IN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: "status",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN,
		Values:   []string{"active"},
	})

	// --- numeric_value_required: LTE/GTE/LT/GT use singular value, not values[] ---

	assertValid(t, "LTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: "age",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		Value:    "30",
	})
	assertValid(t, "GTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: "age",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		Value:    "18",
	})
	assertInvalid(t, "LTE_non_numeric_value_rejected", &commonv1.PropertyFilter{
		Property: "age",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		Value:    "not-a-number",
	})
	assertInvalid(t, "BETWEEN_non_numeric_values_rejected", &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
		Values:   []string{"abc", "50"},
	})

	// --- between_requires_two_values ---

	assertInvalid(t, "BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
		Values:   []string{"10"},
	})
	assertInvalid(t, "NOT_BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
		Values:   []string{},
	})

	// --- value_not_allowed_for_set_operators ---

	assertValid(t, "IS_SET_no_value", &commonv1.PropertyFilter{
		Property: "email",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET,
	})
	assertInvalid(t, "IS_SET_with_value_rejected", &commonv1.PropertyFilter{
		Property: "email",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET,
		Value:    "something",
	})
	assertValid(t, "IS_NOT_SET_no_value", &commonv1.PropertyFilter{
		Property: "email",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET,
	})
}
