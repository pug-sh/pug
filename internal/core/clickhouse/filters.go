package clickhouse

import (
	"fmt"
	"strconv"
	"strings"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

// PropertyExpr returns the ClickHouse expression to resolve a property.
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

// EscapeLike escapes ClickHouse LIKE metacharacters in a value.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// FilterClause builds a single WHERE condition fragment for a PropertyFilter.
func FilterClause(f *commonv1.PropertyFilter) (string, []any, error) {
	return filterClause(f, "")
}

// FilterClauseAliased builds a FilterClause with column references prefixed by alias.
func FilterClauseAliased(f *commonv1.PropertyFilter, alias string) (string, []any, error) {
	return filterClause(f, alias)
}

func filterClause(f *commonv1.PropertyFilter, alias string) (string, []any, error) {
	prop := propertyExpr(f.GetProperty(), alias)

	switch f.GetOperator() {
	case commonv1.FilterOperator_FILTER_OPERATOR_EQUALS:
		return fmt.Sprintf("%s = ?", prop), []any{f.GetValue()}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS:
		return fmt.Sprintf("%s != ?", prop), []any{f.GetValue()}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS:
		return fmt.Sprintf("%s LIKE ?", prop), []any{"%" + EscapeLike(f.GetValue()) + "%"}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS:
		return fmt.Sprintf("%s NOT LIKE ?", prop), []any{"%" + EscapeLike(f.GetValue()) + "%"}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_SET:
		return fmt.Sprintf("%s != ''", prop), nil, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET:
		return fmt.Sprintf("%s = ''", prop), nil, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE:
		n, err := strconv.ParseFloat(f.GetValue(), 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
		}
		return fmt.Sprintf("toFloat64OrNull(%s) <= ?", prop), []any{n}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_GTE:
		n, err := strconv.ParseFloat(f.GetValue(), 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
		}
		return fmt.Sprintf("toFloat64OrNull(%s) >= ?", prop), []any{n}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_LT:
		n, err := strconv.ParseFloat(f.GetValue(), 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
		}
		return fmt.Sprintf("toFloat64OrNull(%s) < ?", prop), []any{n}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_GT:
		n, err := strconv.ParseFloat(f.GetValue(), 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
		}
		return fmt.Sprintf("toFloat64OrNull(%s) > ?", prop), []any{n}, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IN:
		if len(f.GetValues()) == 0 {
			return "", nil, fmt.Errorf("IN operator requires at least one value for property %q", f.GetProperty())
		}
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		return fmt.Sprintf("%s IN (%s)", prop, strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")), args, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN:
		if len(f.GetValues()) == 0 {
			return "", nil, fmt.Errorf("NOT IN operator requires at least one value for property %q", f.GetProperty())
		}
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		return fmt.Sprintf("%s NOT IN (%s)", prop, strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")), args, nil
	default:
		return "", nil, fmt.Errorf("unsupported filter operator: %v", f.GetOperator())
	}
}

// PropertyCondition builds a typed query Condition for a PropertyFilter.
func PropertyCondition(f *commonv1.PropertyFilter) (Condition, error) {
	return propertyCondition(f, "")
}

// PropertyConditionAliased builds a typed query Condition for a PropertyFilter,
// prefixing column references with the given table alias.
func PropertyConditionAliased(f *commonv1.PropertyFilter, alias string) (Condition, error) {
	return propertyCondition(f, alias)
}

func propertyCondition(f *commonv1.PropertyFilter, alias string) (Condition, error) {
	clause, args, err := filterClause(f, alias)
	if err != nil {
		return Condition{}, err
	}
	return RawCond(clause, args...), nil
}

// EventCondition builds a typed query Condition from event filters.
// Empty input returns a zero-value Condition (no-op).
func EventCondition(events []*commonv1.EventFilter) (Condition, error) {
	return eventCondition(events, "")
}

// EventConditionAliased builds a typed query Condition from event filters,
// prefixing column references (kind, auto_properties, custom_properties) with
// the given table alias. Used in JOINed CTEs where bare column names are ambiguous.
func EventConditionAliased(events []*commonv1.EventFilter, alias string) (Condition, error) {
	return eventCondition(events, alias)
}

func eventCondition(events []*commonv1.EventFilter, alias string) (Condition, error) {
	if len(events) == 0 {
		return Condition{}, nil
	}
	if len(events) == 1 {
		return singleEventCondition(events[0], -1, alias)
	}

	conds := make([]Condition, 0, len(events))
	for i, ev := range events {
		cond, err := singleEventCondition(ev, i, alias)
		if err != nil {
			return Condition{}, err
		}
		conds = append(conds, cond)
	}
	return Or(conds...), nil
}

func singleEventCondition(ev *commonv1.EventFilter, idx int, alias string) (Condition, error) {
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
		cond, err := propertyCondition(f, alias)
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

