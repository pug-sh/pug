package clickhouse

import (
	"fmt"
	"strconv"
	"strings"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

// PropertyExpr returns the ClickHouse expression to resolve an event property.
// It checks auto_properties first; if the value is empty or missing, it falls back to custom_properties.
//
// SAFETY: The name is interpolated directly into SQL (not parameterized) because ClickHouse
// map key access requires it. Callers MUST ensure name is validated before calling this function.
// At the RPC boundary, proto validation enforces the pattern ^\$?[a-zA-Z0-9_.-]+$ which
// prevents SQL injection characters. Internal callers outside the RPC chain must validate separately.
func PropertyExpr(name string) string {
	return propertyExpr(name, "")
}

func propertyExpr(name, alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return fmt.Sprintf("ifNull(nullIf(%sauto_properties['%s'], ''), %scustom_properties['%s'])", prefix, name, prefix, name)
}

// ProfilePropertyExpr returns the ClickHouse expression to read a profile property.
// Profile properties are stored in a String column containing JSON, so JSONExtractString is required.
//
// SAFETY: Same interpolation contract as PropertyExpr — name must be proto-validated.
func ProfilePropertyExpr(name string) string {
	return fmt.Sprintf("JSONExtractString(properties, '%s')", name)
}

// EscapeLike escapes ClickHouse LIKE metacharacters in a value.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// PropertyCondition builds a typed query Condition for a PropertyFilter.
// Dispatches to profileFilterCondition when source is PROPERTY_SOURCE_PROFILE.
// For profile filters, projectID is required to build the IN subquery.
func PropertyCondition(f *commonv1.PropertyFilter, projectID string) (Condition, error) {
	return propertyCondition(f, projectID, "")
}

// PropertyConditionAliased builds a typed query Condition for a PropertyFilter,
// prefixing event column references with the given table alias.
// Dispatches to profileFilterCondition when source is PROPERTY_SOURCE_PROFILE.
func PropertyConditionAliased(f *commonv1.PropertyFilter, projectID, alias string) (Condition, error) {
	return propertyCondition(f, projectID, alias)
}

func propertyCondition(f *commonv1.PropertyFilter, projectID, alias string) (Condition, error) {
	if f.GetSource() == commonv1.PropertySource_PROPERTY_SOURCE_PROFILE {
		return profileFilterCondition(projectID, f, alias)
	}
	return eventPropertyCondition(f, alias)
}

// eventPropertyCondition builds a Condition for auto/custom event property filters.
func eventPropertyCondition(f *commonv1.PropertyFilter, alias string) (Condition, error) {
	prop := propertyExpr(f.GetProperty(), alias)
	return operatorCondition(prop, f)
}

// profileFilterCondition builds a Condition that filters events by matching profile properties.
// The alias only prefixes the outer distinct_id reference; subquery columns reference
// profiles/profile_aliases tables directly and are unaffected by the alias.
//
// It generates: [alias.]distinct_id IN (
//
//	SELECT p.id FROM profiles p WHERE p.project_id=? AND p.is_deleted=0 AND p.external_id != '' AND <prop_op>
//	UNION ALL
//	SELECT pa.alias_id FROM profile_aliases pa WHERE pa.project_id=? AND pa.profile_id IN (
//	    SELECT p.id FROM profiles p WHERE p.project_id=? AND p.is_deleted=0 AND p.external_id != '' AND <prop_op>
//	)
//
// )
//
// The external_id guard excludes profiles with empty external_id, which cannot match
// any distinct_id in the events table.
func profileFilterCondition(projectID string, f *commonv1.PropertyFilter, alias string) (Condition, error) {
	if projectID == "" {
		return Condition{}, fmt.Errorf("profile property filter requires a non-empty project ID")
	}
	prop := ProfilePropertyExpr(f.GetProperty())

	innerCond, err := operatorCondition(prop, f)
	if err != nil {
		return Condition{}, err
	}

	distinctIDCol := "distinct_id"
	if alias != "" {
		distinctIDCol = alias + ".distinct_id"
	}

	nonemptyCond := RawCond("p.external_id != ''")
	propertyCond := And(nonemptyCond, innerCond)

	sql := fmt.Sprintf(`%s IN (
		SELECT p.id FROM profiles p WHERE p.project_id = ? AND p.is_deleted = 0 AND %s
		UNION ALL
		SELECT pa.alias_id FROM profile_aliases pa WHERE pa.project_id = ? AND pa.profile_id IN (
			SELECT p.id FROM profiles p WHERE p.project_id = ? AND p.is_deleted = 0 AND %s
		)
	)`, distinctIDCol, propertyCond.SQL(), propertyCond.SQL())

	args := []any{projectID}
	args = append(args, propertyCond.Args()...)
	args = append(args, projectID, projectID)
	args = append(args, propertyCond.Args()...)

	if n := strings.Count(sql, "?"); n != len(args) {
		return Condition{}, fmt.Errorf("profile filter: placeholder count (%d) != arg count (%d)", n, len(args))
	}
	return RawCond(sql, args...), nil
}

// numericCond parses the filter value as float64 and builds a toFloat64OrNull comparison.
func numericCond(prop, op string, f *commonv1.PropertyFilter) (Condition, error) {
	n, err := strconv.ParseFloat(f.GetValue(), 64)
	if err != nil {
		return Condition{}, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
	}
	return RawCond("toFloat64OrNull("+prop+") "+op+" ?", n), nil
}

// operatorCondition builds a Condition for the given SQL expression and PropertyFilter operator.
func operatorCondition(prop string, f *commonv1.PropertyFilter) (Condition, error) {
	switch f.GetOperator() {
	case commonv1.FilterOperator_FILTER_OPERATOR_EQUALS:
		return RawCond(prop+" = ?", f.GetValue()), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS:
		return RawCond(prop+" != ?", f.GetValue()), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS:
		return RawCond(prop+" LIKE ?", "%"+EscapeLike(f.GetValue())+"%"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS:
		return RawCond(prop+" NOT LIKE ?", "%"+EscapeLike(f.GetValue())+"%"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_SET:
		return RawCond(prop + " != ''"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET:
		return RawCond(prop + " = ''"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE:
		return numericCond(prop, "<=", f)
	case commonv1.FilterOperator_FILTER_OPERATOR_GTE:
		return numericCond(prop, ">=", f)
	case commonv1.FilterOperator_FILTER_OPERATOR_LT:
		return numericCond(prop, "<", f)
	case commonv1.FilterOperator_FILTER_OPERATOR_GT:
		return numericCond(prop, ">", f)
	case commonv1.FilterOperator_FILTER_OPERATOR_IN:
		if len(f.GetValues()) == 0 {
			return Condition{}, fmt.Errorf("IN operator requires at least one value for property %q", f.GetProperty())
		}
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		return RawCond(prop+" IN ("+placeholders+")", args...), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN:
		if len(f.GetValues()) == 0 {
			return Condition{}, fmt.Errorf("NOT IN operator requires at least one value for property %q", f.GetProperty())
		}
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		return RawCond(prop+" NOT IN ("+placeholders+")", args...), nil
	default:
		return Condition{}, fmt.Errorf("unsupported filter operator: %v", f.GetOperator())
	}
}

// EventCondition builds a typed query Condition from event filters.
// Empty input returns a zero-value Condition (no-op).
// projectID is required for profile-source property filters within event filters.
func EventCondition(events []*commonv1.EventFilter, projectID string) (Condition, error) {
	return eventCondition(events, projectID, "")
}

// EventConditionAliased builds a typed query Condition from event filters,
// prefixing event table column references (kind, auto_properties, custom_properties) with
// the given table alias. Used in JOINed CTEs where bare column names are ambiguous.
func EventConditionAliased(events []*commonv1.EventFilter, projectID, alias string) (Condition, error) {
	return eventCondition(events, projectID, alias)
}

func eventCondition(events []*commonv1.EventFilter, projectID, alias string) (Condition, error) {
	if len(events) == 0 {
		return Condition{}, nil
	}
	if len(events) == 1 {
		return singleEventCondition(events[0], projectID, -1, alias)
	}

	conds := make([]Condition, 0, len(events))
	for i, ev := range events {
		cond, err := singleEventCondition(ev, projectID, i, alias)
		if err != nil {
			return Condition{}, err
		}
		conds = append(conds, cond)
	}
	return Or(conds...), nil
}

func singleEventCondition(ev *commonv1.EventFilter, projectID string, idx int, alias string) (Condition, error) {
	if ev == nil {
		if idx >= 0 {
			return Condition{}, fmt.Errorf("event[%d]: event filter is nil", idx)
		}
		return Condition{}, fmt.Errorf("event filter is nil")
	}

	kindCol := "kind"
	if alias != "" {
		kindCol = alias + ".kind"
	}

	var conds []Condition
	if ev.GetKind() != "" {
		conds = append(conds, Eq(kindCol, ev.GetKind()))
	}
	for j, f := range ev.GetFilters() {
		cond, err := propertyCondition(f, projectID, alias)
		if err != nil {
			if idx >= 0 {
				return Condition{}, fmt.Errorf("event[%d]: filters[%d]: %w", idx, j, err)
			}
			return Condition{}, fmt.Errorf("event filter: filters[%d]: %w", j, err)
		}
		conds = append(conds, cond)
	}

	if len(conds) == 0 {
		if idx >= 0 {
			return Condition{}, fmt.Errorf("event[%d]: empty event filter in multi-event query", idx)
		}
		return Condition{}, nil
	}

	return And(conds...), nil
}
