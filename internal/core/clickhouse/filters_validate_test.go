package clickhouse_test

import (
	"testing"

	"buf.build/go/protovalidate"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

func strp(s string) *string         { return &s }
func opp(o commonv1.FilterOperator) *commonv1.FilterOperator { return &o }

// assertValid asserts that the given PropertyFilter passes proto validation.
func assertValid(t *testing.T, name string, f *commonv1.PropertyFilter) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := protovalidate.Validate(f); err != nil {
			t.Errorf("expected valid, got error: %v", err)
		}
	})
}

// assertInvalid asserts that the given PropertyFilter fails proto validation.
func assertInvalid(t *testing.T, name string, f *commonv1.PropertyFilter) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := protovalidate.Validate(f); err == nil {
			t.Error("expected validation error, got nil")
		}
	})
}

// TestFilterOperatorValidation exercises the required + defined_only constraints on FilterOperator.
func TestFilterOperatorValidation(t *testing.T) {
	assertInvalid(t, "unspecified_rejected", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_UNSPECIFIED),
		Value:    strp("US"),
	})
	assertInvalid(t, "undefined_value_rejected", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(999),
		Value:    strp("US"),
	})
	assertValid(t, "valid_equals", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_EQUALS),
		Value:    strp("US"),
	})
}

// TestPropertyFilterValidation exercises all CEL validation rules on PropertyFilter:
// value_required, numeric_value_required, values_required, values_not_allowed,
// between_requires_two_values, between_ordered_values, and value_not_allowed_for_set_operators.
func TestPropertyFilterValidation(t *testing.T) {
	// --- values_not_allowed regression: operators that use values[] must be accepted ---

	assertValid(t, "IN_with_values", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IN),
		Values:   []string{"US", "UK"},
	})
	assertValid(t, "NOT_IN_with_values", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN),
		Values:   []string{"CN"},
	})
	assertValid(t, "BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10", "50"},
	})
	assertValid(t, "NOT_BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: strp("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"0", "100"},
	})

	// --- values_not_allowed: operators that don't use values[] must reject non-empty values[] ---

	assertInvalid(t, "EQUALS_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: strp("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_EQUALS),
		Value:    strp("US"),
		Values:   []string{"extra"},
	})
	assertInvalid(t, "GTE_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_GTE),
		Value:    strp("5"),
		Values:   []string{"1", "2"},
	})

	// --- value_required: operators that use values[] must not require a singular value ---

	assertValid(t, "BETWEEN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: strp("price"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"1", "9"},
	})
	assertValid(t, "IN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: strp("status"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IN),
		Values:   []string{"active"},
	})

	// --- numeric_value_required: LTE/GTE/LT/GT use singular value, not values[] ---

	assertValid(t, "LTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: strp("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_LTE),
		Value:    strp("30"),
	})
	assertValid(t, "GTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: strp("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_GTE),
		Value:    strp("18"),
	})
	assertInvalid(t, "LTE_non_numeric_value_rejected", &commonv1.PropertyFilter{
		Property: strp("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_LTE),
		Value:    strp("not-a-number"),
	})
	assertInvalid(t, "BETWEEN_non_numeric_values_rejected", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"abc", "50"},
	})

	// --- between_requires_two_values ---

	assertInvalid(t, "BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10"},
	})
	assertInvalid(t, "NOT_BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{},
	})
	assertInvalid(t, "BETWEEN_three_values_rejected", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10", "50", "90"},
	})
	assertInvalid(t, "NOT_BETWEEN_three_values_rejected", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"10", "50", "90"},
	})

	// --- between_ordered_values: values[0] must be <= values[1] ---

	assertValid(t, "BETWEEN_equal_values_accepted", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"50", "50"},
	})
	assertInvalid(t, "BETWEEN_reversed_values_rejected", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"100", "10"},
	})
	assertInvalid(t, "NOT_BETWEEN_reversed_values_rejected", &commonv1.PropertyFilter{
		Property: strp("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"99.5", "1.5"},
	})
	assertValid(t, "NOT_BETWEEN_ordered_values_accepted", &commonv1.PropertyFilter{
		Property: strp("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"1.5", "99.5"},
	})

	// --- numeric_value_required: NOT_BETWEEN coverage ---

	assertInvalid(t, "NOT_BETWEEN_non_numeric_values_rejected", &commonv1.PropertyFilter{
		Property: strp("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"abc", "50"},
	})
	assertInvalid(t, "BETWEEN_second_value_non_numeric_rejected", &commonv1.PropertyFilter{
		Property: strp("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"50", "abc"},
	})
	assertInvalid(t, "NOT_BETWEEN_second_value_non_numeric_rejected", &commonv1.PropertyFilter{
		Property: strp("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"50", "abc"},
	})

	// --- value_not_allowed_for_set_operators ---

	assertValid(t, "IS_SET_no_value", &commonv1.PropertyFilter{
		Property: strp("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_SET),
	})
	assertInvalid(t, "IS_SET_with_value_rejected", &commonv1.PropertyFilter{
		Property: strp("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_SET),
		Value:    strp("something"),
	})
	assertValid(t, "IS_NOT_SET_no_value", &commonv1.PropertyFilter{
		Property: strp("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET),
	})
}
