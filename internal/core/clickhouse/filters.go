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
	return fmt.Sprintf("ifNull(nullIf(auto_properties['%s'], ''), custom_properties['%s'])", name, name)
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
	prop := PropertyExpr(f.GetProperty())

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

// WriteEventFilterCondition appends OR-joined event kind + per-event filter
// conditions to sb/args. No-op when events is empty.
// Single event:   AND kind = ? [AND per-event filters...]
// Multiple events: AND ((kind=? AND ...) OR (kind=? AND ...))
//
// On error, the state of sb and args is undefined; callers must not use them.
func WriteEventFilterCondition(sb *strings.Builder, args *[]any, events []*commonv1.EventFilter) error {
	if len(events) == 0 {
		return nil
	}
	if len(events) == 1 {
		ev := events[0]
		if ev.GetKind() != "" {
			sb.WriteString("AND kind = ?\n")
			*args = append(*args, ev.GetKind())
		}
		for _, f := range ev.GetFilters() {
			clause, fArgs, err := FilterClause(f)
			if err != nil {
				return fmt.Errorf("event filter: %w", err)
			}
			sb.WriteString("AND ")
			sb.WriteString(clause)
			sb.WriteString("\n")
			*args = append(*args, fArgs...)
		}
		return nil
	}
	// Multiple events: OR-join each event's conditions.
	sb.WriteString("AND (\n")
	for i, ev := range events {
		if i > 0 {
			sb.WriteString("OR ")
		}
		var parts []string
		var evArgs []any
		if ev.GetKind() != "" {
			parts = append(parts, "kind = ?")
			evArgs = append(evArgs, ev.GetKind())
		}
		for _, f := range ev.GetFilters() {
			clause, fArgs, err := FilterClause(f)
			if err != nil {
				return fmt.Errorf("event[%d]: %w", i, err)
			}
			parts = append(parts, clause)
			evArgs = append(evArgs, fArgs...)
		}
		if len(parts) == 0 {
			return fmt.Errorf("event[%d]: empty event filter in multi-event query", i)
		}
		sb.WriteString("(")
		sb.WriteString(strings.Join(parts, " AND "))
		sb.WriteString(")\n")
		*args = append(*args, evArgs...)
	}
	sb.WriteString(")\n")
	return nil
}
